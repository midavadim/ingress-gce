/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package readiness

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	negtypes "k8s.io/ingress-gce/pkg/neg/types"
	"k8s.io/ingress-gce/pkg/neg/types/shared"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/zonegetter"
	"k8s.io/klog/v2"
	clocktesting "k8s.io/utils/clock/testing"
)

const (
	defaultTestSubnetURL = "https://www.googleapis.com/compute/v1/projects/mock-project/regions/test-region/subnetworks/default"

	defaultTestSubnet    = "default"
	nonDefaultTestSubnet = "non-default"

	testServiceNamespace = "test-ns"
)

// fakeLookUp implements LookUp interface
type fakeLookUp struct {
	readinessGateEnabled     bool
	readinessGateEnabledNegs []string
}

func (f *fakeLookUp) ReadinessGateEnabledNegs(namespace string, labels map[string]string) []string {
	return f.readinessGateEnabledNegs
}

// ReadinessGateEnabled returns true if the NEG requires readiness feedback
func (f *fakeLookUp) ReadinessGateEnabled(syncerKey negtypes.NegSyncerKey) bool {
	return f.readinessGateEnabled
}

func newTestReadinessReflector(testContext *negtypes.TestContext, markNonDefaultSubnetPodsReady bool) (*readinessReflector, error) {
	fakeZoneGetter, err := zonegetter.NewFakeZoneGetter(testContext.NodeInformer, testContext.NodeTopologyInformer, defaultTestSubnetURL, markNonDefaultSubnetPodsReady)
	if err != nil {
		return nil, err
	}
	reflector := NewReadinessReflector(
		testContext.KubeClient,
		testContext.KubeClient,
		testContext.PodInformer.GetIndexer(),
		negtypes.NewAdapter(testContext.Cloud),
		&fakeLookUp{},
		fakeZoneGetter,
		false,
		markNonDefaultSubnetPodsReady,
		klog.TODO(),
	)
	ret := reflector.(*readinessReflector)
	return ret, nil
}

func TestSyncPod(t *testing.T) {
	t.Parallel()
	fakeContext := negtypes.NewTestContext()
	client := fakeContext.KubeClient
	podLister := fakeContext.PodInformer.GetIndexer()
	nodeLister := fakeContext.NodeInformer.GetIndexer()
	fakeClock := clocktesting.NewFakeClock(time.Now())
	testReadinessReflector, err := newTestReadinessReflector(fakeContext, false)
	if err != nil {
		t.Fatalf("failed to initialize readiness reflector")
	}
	testReadinessReflector.clock = fakeClock
	testlookUp := testReadinessReflector.lookup.(*fakeLookUp)

	nodeName := "instance1"
	now := metav1.NewTime(fakeClock.Now()).Rfc3339Copy()
	// Set creation time to be some time in the past so it exceeds unreadyTimeout.
	timeoutTime := metav1.NewTime(fakeClock.Now().Add(-unreadyTimeout)).Rfc3339Copy()

	nodeLister.Add(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Spec: v1.NodeSpec{
			PodCIDR:  "10.100.1.0/24",
			PodCIDRs: []string{"a:b::/48", "10.100.1.0/24"},
		},
	})
	zonegetter.PopulateFakeNodeInformer(fakeContext.NodeInformer, false)

	testCases := []struct {
		desc                string
		podName             string
		mutateState         func(*fakeLookUp)
		inputKey            string
		inputNeg            *meta.Key
		inputBackendService *meta.Key
		expectExists        bool
		expectPod           *v1.Pod
	}{
		{
			desc: "empty input",
			mutateState: func(testlookUp *fakeLookUp) {
				testlookUp.readinessGateEnabledNegs = []string{}
			},
			expectExists: false,
		},
		{
			desc: "no need to update when pod does not have neg readiness gate",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, "pod1", false, true, true)
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
				testlookUp.readinessGateEnabledNegs = []string{}
			},
			inputKey:     keyFunc(testServiceNamespace, "pod1"),
			inputNeg:     nil,
			expectExists: true,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      "pod1",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:   shared.NegReadinessGate,
							Status: v1.ConditionTrue,
							Reason: negReadyReason,
						},
					},
				},
			},
		},
		{
			desc: "no need to update 2 when pod already has neg condition status == true",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, "pod2", true, true, true)
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
				testlookUp.readinessGateEnabledNegs = []string{}
			},
			inputKey:     keyFunc(testServiceNamespace, "pod2"),
			inputNeg:     nil,
			expectExists: true,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      "pod2",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:   shared.NegReadinessGate,
							Status: v1.ConditionTrue,
							Reason: negReadyReason,
						},
					},
				},
			},
		},
		{
			desc: "need to update pod but there is no Negs associated",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, "pod3", true, false, false)
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
				testlookUp.readinessGateEnabledNegs = []string{}
			},
			inputKey:     keyFunc(testServiceNamespace, "pod3"),
			inputNeg:     nil,
			expectExists: true,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      "pod3",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Status:  v1.ConditionTrue,
							Reason:  negReadyReason,
							Message: fmt.Sprintf("Pod does not belong to any NEG. Marking condition %q to True.", shared.NegReadinessGate),
						},
					},
				},
			},
		},
		{
			desc: "need to update pod: there is NEGs associated but pod is not healthy",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, "pod4", true, false, false)
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
				testlookUp.readinessGateEnabledNegs = []string{"neg1", "neg2"}
			},
			inputKey:     keyFunc(testServiceNamespace, "pod4"),
			inputNeg:     nil,
			expectExists: true,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      "pod4",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Reason:  negNotReadyReason,
							Message: fmt.Sprintf("Waiting for pod to become healthy in at least one of the NEG(s): %v", []string{"neg1", "neg2"}),
						},
					},
				},
			},
		},
		{
			desc: "need to update pod: pod is not attached to health check",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, "pod5", true, false, false)
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
				testlookUp.readinessGateEnabledNegs = []string{"neg1", "neg2"}
			},
			inputKey:            keyFunc(testServiceNamespace, "pod5"),
			inputNeg:            meta.ZonalKey("neg1", "zone1"),
			inputBackendService: nil,
			expectExists:        true,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      "pod5",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Reason:  negReadyUnhealthCheckedReason,
							Status:  v1.ConditionTrue,
							Message: fmt.Sprintf("Pod is in NEG %q. NEG is not attached to any BackendService with health checking. Marking condition %q to True.", meta.ZonalKey("neg1", "zone1").String(), shared.NegReadinessGate),
						},
					},
				},
			},
		},
		{
			desc: "timeout waiting for endpoint to become healthy in NEGs",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, "pod6", true, false, false)
				pod.CreationTimestamp = timeoutTime
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
				testlookUp.readinessGateEnabledNegs = []string{"neg1", "neg2"}
			},
			inputKey:     keyFunc(testServiceNamespace, "pod6"),
			inputNeg:     nil,
			expectExists: true,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      "pod6",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Reason:  negReadyTimedOutReason,
							Status:  v1.ConditionTrue,
							Message: fmt.Sprintf("Timeout waiting for pod to become healthy in at least one of the NEG(s): %v. Marking condition %q to True.", []string{"neg1", "neg2"}, shared.NegReadinessGate)},
					},
				},
			},
		},
		{
			desc: "need to update pod: pod is healthy in NEG ",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, "pod7", true, false, false)
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
				testlookUp.readinessGateEnabledNegs = []string{"neg1", "neg2"}
			},
			inputKey:            keyFunc(testServiceNamespace, "pod7"),
			inputNeg:            meta.ZonalKey("neg1", "zone1"),
			inputBackendService: meta.GlobalKey("k8s-backendservice"),
			expectExists:        true,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      "pod7",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Reason:  negReadyReason,
							Status:  v1.ConditionTrue,
							Message: fmt.Sprintf("Pod has become Healthy in NEG %q attached to BackendService %q. Marking condition %q to True.", meta.ZonalKey("neg1", "zone1").String(), meta.GlobalKey("k8s-backendservice").String(), shared.NegReadinessGate),
						},
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			for _, markNonDefaultSubnetPodsReady := range []bool{true, false} {
				testReadinessReflector.markNonDefaultSubnetPodsReady = markNonDefaultSubnetPodsReady
				tc.mutateState(testlookUp)
				err := testReadinessReflector.syncPod(tc.inputKey, tc.inputNeg, tc.inputBackendService)
				if err != nil {
					t.Errorf("For test case %q with markNonDefaultSubnetPodsReady = %v, expect syncPod() return nil, but got %v", tc.desc, markNonDefaultSubnetPodsReady, err)
				}

				if tc.expectExists {
					pod, err := fakeContext.KubeClient.CoreV1().Pods(testServiceNamespace).Get(context.TODO(), tc.expectPod.Name, metav1.GetOptions{})
					if err != nil {
						t.Errorf("For test case %q with markNonDefaultSubnetPodsReady = %v, expect GetPod() to return nil, but got %v", tc.desc, markNonDefaultSubnetPodsReady, err)
					}
					// ignore creation timestamp for comparison
					pod.CreationTimestamp = tc.expectPod.CreationTimestamp
					if !reflect.DeepEqual(pod, tc.expectPod) {
						t.Errorf("For test case %q with markNonDefaultSubnetPodsReady = %v, expect pod to be %v, but got %v", tc.desc, markNonDefaultSubnetPodsReady, tc.expectPod, pod)
					}
				}
			}
		})
	}
}

func TestSyncPodMultipleSubnets(t *testing.T) {
	t.Parallel()
	fakeContext := negtypes.NewTestContext()
	client := fakeContext.KubeClient
	podLister := fakeContext.PodInformer.GetIndexer()
	fakeClock := clocktesting.NewFakeClock(time.Now())
	testReadinessReflector, err := newTestReadinessReflector(fakeContext, true)
	if err != nil {
		t.Fatalf("failed to initialize readiness reflector")
	}
	testReadinessReflector.clock = fakeClock
	testlookUp := testReadinessReflector.lookup.(*fakeLookUp)
	testlookUp.readinessGateEnabledNegs = []string{"neg1", "neg2"}

	now := metav1.NewTime(fakeClock.Now()).Rfc3339Copy()
	// Set creation time to be some time in the past so it exceeds unreadyTimeout.
	timeoutTime := metav1.NewTime(fakeClock.Now().Add(-unreadyTimeout)).Rfc3339Copy()
	nodeMissingPod := "node-missing-pod"

	zonegetter.PopulateFakeNodeInformer(fakeContext.NodeInformer, true)

	testCases := []struct {
		desc                string
		podName             string
		mutateState         func(*fakeLookUp)
		inputKey            string
		inputNeg            *meta.Key
		inputBackendService *meta.Key
		expectPod           *v1.Pod
	}{
		{
			desc: "need to update pod: pod belongs to a node in non-default subnet",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, negtypes.TestNonDefaultSubnetPod, true, false, false)
				pod.Spec.NodeName = negtypes.TestNonDefaultSubnetInstance
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
			},
			inputKey: keyFunc(testServiceNamespace, negtypes.TestNonDefaultSubnetPod),
			inputNeg: nil,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      negtypes.TestNonDefaultSubnetPod,
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: negtypes.TestNonDefaultSubnetInstance,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Status:  v1.ConditionTrue,
							Reason:  negReadyReason,
							Message: fmt.Sprintf("Pod belongs to a node in non-default subnet. Marking condition %q to True.", shared.NegReadinessGate),
						},
					},
				},
			},
		},
		{
			desc: "need to update pod: pod belongs to a node without PodCIDR, timeout not reached yet",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, negtypes.TestNoPodCIDRPod+"-1", true, false, false)
				pod.Spec.NodeName = negtypes.TestNoPodCIDRInstance
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
			},
			inputKey: keyFunc(testServiceNamespace, negtypes.TestNoPodCIDRPod+"-1"),
			inputNeg: nil,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      negtypes.TestNoPodCIDRPod + "-1",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: negtypes.TestNoPodCIDRInstance,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Reason:  negNotReadyReason,
							Message: "Unable to determine if the pod is in the default subnet.",
						},
					},
				},
			},
		},
		{
			desc: "need to update pod: pod belongs to a node without PodCIDR, timeout reached",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, negtypes.TestNoPodCIDRPod+"-2", true, false, false)
				pod.Spec.NodeName = negtypes.TestNoPodCIDRInstance
				pod.CreationTimestamp = timeoutTime
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
			},
			inputKey: keyFunc(testServiceNamespace, negtypes.TestNoPodCIDRPod+"-2"),
			inputNeg: nil,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      negtypes.TestNoPodCIDRPod + "-2",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: negtypes.TestNoPodCIDRInstance,
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Reason:  negReadyTimedOutReason,
							Status:  v1.ConditionTrue,
							Message: fmt.Sprintf("Timeout waiting for pod to become healthy in at least one of the NEG(s): %v. Marking condition %q to True.", []string{"neg1", "neg2"}, shared.NegReadinessGate)},
					},
				},
			},
		},
		{
			desc: "need to update pod: pod does not have nodeName specified, timeout not reached yet",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, nodeMissingPod+"-1", true, false, false)
				pod.Spec.NodeName = ""
				pod.CreationTimestamp = now
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
			},
			inputKey: keyFunc(testServiceNamespace, nodeMissingPod+"-1"),
			inputNeg: nil,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      nodeMissingPod + "-1",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: "",
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Reason:  negNotReadyReason,
							Message: "Unable to determine the pod's node name.",
						},
					},
				},
			},
		},
		{
			desc: "need to update pod: pod does not have nodeName specified, timeout reached",
			mutateState: func(testlookUp *fakeLookUp) {
				pod := generatePod(testServiceNamespace, nodeMissingPod+"-2", true, false, false)
				pod.Spec.NodeName = ""
				pod.CreationTimestamp = timeoutTime
				podLister.Add(pod)
				client.CoreV1().Pods(testServiceNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
			},
			inputKey: keyFunc(testServiceNamespace, nodeMissingPod+"-2"),
			inputNeg: nil,
			expectPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: testServiceNamespace,
					Name:      nodeMissingPod + "-2",
					Labels: map[string]string{
						utils.LabelNodeSubnet: defaultTestSubnet,
					},
				},
				Spec: v1.PodSpec{
					NodeName: "",
					ReadinessGates: []v1.PodReadinessGate{
						{ConditionType: shared.NegReadinessGate},
					},
				},
				Status: v1.PodStatus{
					Conditions: []v1.PodCondition{
						{
							Type:    shared.NegReadinessGate,
							Reason:  negReadyTimedOutReason,
							Status:  v1.ConditionTrue,
							Message: fmt.Sprintf("Timeout waiting for pod to become healthy in at least one of the NEG(s): %v. Marking condition %q to True.", []string{"neg1", "neg2"}, shared.NegReadinessGate)},
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			tc.mutateState(testlookUp)
			err := testReadinessReflector.syncPod(tc.inputKey, tc.inputNeg, tc.inputBackendService)
			if err != nil {
				t.Errorf("For test case %q with multi-subnet cluster enabled, expect err to be nil, but got %v", tc.desc, err)
			}

			pod, err := fakeContext.KubeClient.CoreV1().Pods(testServiceNamespace).Get(context.TODO(), tc.expectPod.Name, metav1.GetOptions{})
			if err != nil {
				t.Errorf("For test case %q with multi-subnet cluster enabled, expect err to be nil, but got %v", tc.desc, err)
			}
			// ignore creation timestamp for comparison
			pod.CreationTimestamp = tc.expectPod.CreationTimestamp
			if diff := cmp.Diff(tc.expectPod, pod); diff != "" {
				t.Errorf("For test case %q with multi-subnet cluster enabled, found diff in unexpected state of pod; (-want,+got)=\n%v", tc.desc, diff)
			}
		})
	}
}
