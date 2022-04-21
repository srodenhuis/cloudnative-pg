/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2022 EnterpriseDB Corporation.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/specs"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/utils"
	"github.com/EnterpriseDB/cloud-native-postgresql/tests"
	testUtils "github.com/EnterpriseDB/cloud-native-postgresql/tests/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Fencing", func() {
	const (
		sampleFile = fixturesDir + "/base/cluster-storage-class.yaml"
		level      = tests.Medium
	)
	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
	})
	var namespace, clusterName string
	var pod corev1.Pod

	JustAfterEach(func() {
		if CurrentSpecReport().Failed() {
			env.DumpClusterEnv(namespace, clusterName,
				"out/"+CurrentSpecReport().LeafNodeText+".log")
		}
	})

	checkInstanceStatusReadyOrNot := func(instanceName, namespace string, isReady bool) {
		var pod corev1.Pod
		Eventually(func() (bool, error) {
			err := env.Client.Get(env.Ctx,
				ctrlclient.ObjectKey{Namespace: namespace, Name: instanceName},
				&pod)
			if err != nil {
				return false, err
			}
			for _, podInfo := range pod.Status.ContainerStatuses {
				if podInfo.Name == specs.PostgresContainerName {
					if podInfo.Ready == isReady {
						return true, nil
					}
				}
			}
			return false, nil
		}, 120, 5).Should(BeTrue())
	}

	checkInstanceIsStreaming := func(instanceName, namespace string) {
		timeout := time.Second
		Eventually(func() (int, error) {
			err := env.Client.Get(env.Ctx,
				ctrlclient.ObjectKey{Namespace: namespace, Name: instanceName},
				&pod)
			if err != nil {
				return 0, err
			}
			out, _, err := env.ExecCommand(env.Ctx, pod, specs.PostgresContainerName, &timeout,
				"psql", "-U", "postgres", "-tAc", "SELECT count(*) FROM pg_stat_wal_receiver")
			if err != nil {
				return 0, err
			}
			value, atoiErr := strconv.Atoi(strings.Trim(out, "\n"))
			return value, atoiErr
		}, 60).Should(BeEquivalentTo(1))
	}

	checkPostgresConnection := func(podName, namespace string) {
		err := testUtils.GetObject(env, ctrlclient.ObjectKey{Namespace: namespace, Name: podName}, &pod)
		Expect(err).ToNot(HaveOccurred())
		timeout := time.Second * 2
		dsn := fmt.Sprintf("host=%v user=%v dbname=%v password=%v sslmode=require",
			testUtils.PGLocalSocketDir, "postgres", "postgres", "")
		stdOut, stdErr, err := utils.ExecCommand(env.Ctx, env.Interface, env.RestClientConfig, pod,
			specs.PostgresContainerName, &timeout,
			"psql", dsn, "-tAc", "SELECT 1")
		Expect(err).To(HaveOccurred(), stdErr, stdOut)
	}

	checkFencingAnnotationSet := func(fencingMethod testUtils.FencingMethod, content []string) {
		if fencingMethod != testUtils.UsingAnnotation {
			return
		}
		By("checking the cluster has the expected annotation set", func() {
			cluster, err := env.GetCluster(namespace, clusterName)
			Expect(err).NotTo(HaveOccurred())
			Expect(cluster.Annotations).NotTo(BeNil())
			if len(content) == 0 {
				Expect(cluster.Annotations).To(Or(Not(HaveKey(utils.FencedInstanceAnnotation)),
					HaveKeyWithValue(utils.FencedInstanceAnnotation, "")))
				return
			}
			fencedInstances := make([]string, 0, len(content))
			Expect(json.Unmarshal([]byte(cluster.Annotations[utils.FencedInstanceAnnotation]), &fencedInstances)).
				NotTo(HaveOccurred())
			Expect(fencedInstances).To(BeEquivalentTo(content))
		})
	}

	assertFencingPrimaryWorks := func(fencingMethod testUtils.FencingMethod) {
		It("can fence a primary instance", func() {
			var beforeFencingPodName string
			By("fencing the primary instance", func() {
				primaryPod, err := env.GetClusterPrimary(namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				beforeFencingPodName = primaryPod.GetName()
				Expect(testUtils.FencingOn(env, beforeFencingPodName,
					namespace, clusterName, fencingMethod)).ToNot(HaveOccurred())
			})
			By("check the instance is not ready, but kept as primary instance", func() {
				checkInstanceStatusReadyOrNot(beforeFencingPodName, namespace, false)
				currentPrimaryPodInfo, err := env.GetClusterPrimary(namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(beforeFencingPodName).To(Equal(currentPrimaryPodInfo.GetName()))
			})
			checkFencingAnnotationSet(fencingMethod, []string{beforeFencingPodName})

			By("check postgres connection on primary", func() {
				checkPostgresConnection(beforeFencingPodName, namespace)
			})
			By("lift the fencing", func() {
				Expect(testUtils.FencingOff(env, beforeFencingPodName,
					namespace, clusterName, fencingMethod)).ToNot(HaveOccurred())
			})
			By("the old primary becomes ready", func() {
				checkInstanceStatusReadyOrNot(beforeFencingPodName, namespace, true)
			})
			By("the old primary should still be the primary instance", func() {
				currentPrimaryPodInfo, err := env.GetClusterPrimary(namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(beforeFencingPodName).Should(BeEquivalentTo(currentPrimaryPodInfo.GetName()))
			})
			By("all followers should be streaming again from the primary instance", func() {
				assertClusterStandbysAreStreaming(namespace, clusterName)
			})
			checkFencingAnnotationSet(fencingMethod, nil)
		})
	}
	assertFencingFollowerWorks := func(fencingMethod testUtils.FencingMethod) {
		It("can fence a follower instance", func() {
			var beforeFencingPodName string
			AssertClusterIsReady(namespace, clusterName, 120, env)
			By("fence a follower instance", func() {
				podList, _ := env.GetClusterPodList(namespace, clusterName)
				Expect(len(podList.Items)).To(BeEquivalentTo(3))
				for _, pod := range podList.Items {
					if specs.IsPodStandby(pod) {
						beforeFencingPodName = pod.Name
						break
					}
				}
				Expect(beforeFencingPodName).ToNot(BeEmpty())
				Expect(testUtils.FencingOn(env, beforeFencingPodName,
					namespace, clusterName, fencingMethod)).ToNot(HaveOccurred())
			})
			checkFencingAnnotationSet(fencingMethod, []string{beforeFencingPodName})

			By("check the instance is not ready", func() {
				checkInstanceStatusReadyOrNot(beforeFencingPodName, namespace, false)
			})
			By("check postgres connection follower instance", func() {
				checkPostgresConnection(beforeFencingPodName, namespace)
			})
			By("lift the fencing", func() {
				Expect(testUtils.FencingOff(env, beforeFencingPodName,
					namespace, clusterName, fencingMethod)).ToNot(HaveOccurred())
			})
			By("the instance becomes ready", func() {
				checkInstanceStatusReadyOrNot(beforeFencingPodName, namespace, true)
			})
			By("the instance is streaming again from the primary", func() {
				checkInstanceIsStreaming(beforeFencingPodName, namespace)
			})
			checkFencingAnnotationSet(fencingMethod, nil)
		})
	}
	assertFencingClusterWorks := func(fencingMethod testUtils.FencingMethod) {
		It("can fence all the instances in a cluster", func() {
			primaryPod, err := env.GetClusterPrimary(namespace, clusterName)
			Expect(err).ToNot(HaveOccurred())
			primaryPodName := primaryPod.GetName()
			By("fence the whole cluster using \"(*)\"", func() {
				Expect(testUtils.FencingOn(env, "*", namespace, clusterName, fencingMethod)).ToNot(HaveOccurred())
			})
			checkFencingAnnotationSet(fencingMethod, []string{"*"})
			By("check all instances are not ready", func() {
				podList, err := env.GetClusterPodList(namespace, clusterName)
				Expect(err).NotTo(HaveOccurred())
				for _, pod := range podList.Items {
					checkInstanceStatusReadyOrNot(pod.GetName(), namespace, false)
				}
			})
			By("check postgres connection on all instances", func() {
				podList, err := env.GetClusterPodList(namespace, clusterName)
				Expect(err).NotTo(HaveOccurred())
				for _, pod := range podList.Items {
					checkPostgresConnection(pod.GetName(), namespace)
				}
			})
			By("lift the fencing", func() {
				Expect(testUtils.FencingOff(env, "*", namespace, clusterName, fencingMethod)).ToNot(HaveOccurred())
			})
			By("all instances become ready", func() {
				podList, err := env.GetClusterPodList(namespace, clusterName)
				Expect(err).NotTo(HaveOccurred())
				for _, pod := range podList.Items {
					checkInstanceStatusReadyOrNot(pod.GetName(), namespace, true)
				}
			})
			By("the old primary is still the primary instance", func() {
				podName, err := env.GetClusterPrimary(namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(primaryPodName).Should(BeEquivalentTo(podName.GetName()))
			})
			By("cluster functionality are back", func() {
				AssertClusterIsReady(namespace, clusterName, 30, env)
			})
			checkFencingAnnotationSet(fencingMethod, nil)
		})
	}

	Context("using kubectl-cnp plugin", Ordered, func() {
		var err error
		BeforeAll(func() {
			namespace = "fencing-using-plugin"
			clusterName, err = env.GetResourceNameFromYAML(sampleFile)
			Expect(err).ToNot(HaveOccurred())
			// Create a cluster in a namespace we'll delete after the test
			err = env.CreateNamespace(namespace)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)
		})
		AfterAll(func() {
			err := env.DeleteNamespace(namespace)
			Expect(err).ToNot(HaveOccurred())
		})
		assertFencingPrimaryWorks(testUtils.UsingPlugin)
		assertFencingFollowerWorks(testUtils.UsingPlugin)
		assertFencingClusterWorks(testUtils.UsingPlugin)
	})

	Context("using annotation", Ordered, func() {
		var err error
		BeforeAll(func() {
			namespace = "fencing-using-annotation"
			clusterName, err = env.GetResourceNameFromYAML(sampleFile)
			Expect(err).ToNot(HaveOccurred())
			// Create a cluster in a namespace we'll delete after the test
			err = env.CreateNamespace(namespace)
			Expect(err).ToNot(HaveOccurred())
			AssertCreateCluster(namespace, clusterName, sampleFile, env)
		})
		AfterAll(func() {
			err := env.DeleteNamespace(namespace)
			Expect(err).ToNot(HaveOccurred())
		})
		assertFencingPrimaryWorks(testUtils.UsingAnnotation)
		assertFencingFollowerWorks(testUtils.UsingAnnotation)
		assertFencingClusterWorks(testUtils.UsingAnnotation)
	})
})