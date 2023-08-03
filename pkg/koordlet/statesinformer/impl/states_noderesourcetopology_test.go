/*
Copyright 2022 The Koordinator Authors.

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

package impl

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/golang/mock/gomock"
	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	faketopologyclientset "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned/fake"
	topologylister "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/listers/topology/v1alpha1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/utils/pointer"

	"github.com/koordinator-sh/koordinator/apis/extension"
	fakekoordclientset "github.com/koordinator-sh/koordinator/pkg/client/clientset/versioned/fake"
	"github.com/koordinator-sh/koordinator/pkg/features"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/metriccache"
	mock_metriccache "github.com/koordinator-sh/koordinator/pkg/koordlet/metriccache/mockmetriccache"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/statesinformer"
	koordletutil "github.com/koordinator-sh/koordinator/pkg/koordlet/util"
	"github.com/koordinator-sh/koordinator/pkg/util"
)

var _ topologylister.NodeResourceTopologyLister = &fakeNodeResourceTopologyLister{}

type fakeNodeResourceTopologyLister struct {
	nodeResourceTopologys *topologyv1alpha1.NodeResourceTopology
	getErr                error
}

func (f fakeNodeResourceTopologyLister) List(selector labels.Selector) (ret []*topologyv1alpha1.NodeResourceTopology, err error) {
	return []*topologyv1alpha1.NodeResourceTopology{f.nodeResourceTopologys}, nil
}

func (f fakeNodeResourceTopologyLister) Get(name string) (*topologyv1alpha1.NodeResourceTopology, error) {
	return f.nodeResourceTopologys, f.getErr
}

func Test_syncNodeResourceTopology(t *testing.T) {
	client := faketopologyclientset.NewSimpleClientset()
	testNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
		},
	}
	r := &nodeTopoInformer{
		topologyClient: client,
		nodeResourceTopologyLister: &fakeNodeResourceTopologyLister{
			nodeResourceTopologys: &topologyv1alpha1.NodeResourceTopology{
				ObjectMeta: metav1.ObjectMeta{},
			},
			getErr: errors.NewNotFound(schema.GroupResource{}, "test"),
		},
		nodeInformer: &nodeInformer{
			node: testNode,
		},
	}
	r.createNodeTopoIfNotExist()

	topologyName := testNode.Name

	topology, err := client.TopologyV1alpha1().NodeResourceTopologies().Get(context.TODO(), topologyName, metav1.GetOptions{})

	assert.Equal(t, nil, err)
	assert.Equal(t, topologyName, topology.Name)
	assert.Equal(t, "Koordinator", topology.Labels[extension.LabelManagedBy])
}

func Test_nodeResourceTopology_NewAndSetup(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	type args struct {
		ctx   *PluginOption
		state *PluginState
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "new and setup node topo",
			args: args{
				ctx: &PluginOption{
					config:      NewDefaultConfig(),
					KubeClient:  fakeclientset.NewSimpleClientset(),
					KoordClient: fakekoordclientset.NewSimpleClientset(),
					TopoClient:  faketopologyclientset.NewSimpleClientset(),
					NodeName:    "test-node",
				},
				state: &PluginState{
					metricCache: mock_metriccache.NewMockMetricCache(ctrl),
					informerPlugins: map[PluginName]informerPlugin{
						podsInformerName: NewPodsInformer(),
						nodeInformerName: NewNodeInformer(),
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewNodeTopoInformer()
			r.Setup(tt.args.ctx, tt.args.state)
		})
	}
}

func Test_calGuaranteedCpu(t *testing.T) {
	testCases := []struct {
		name              string
		podMap            map[string]*statesinformer.PodMeta
		checkpointContent string
		expectedError     bool
		expectedPodAllocs []extension.PodCPUAlloc
	}{
		{
			name:              "Restore non-existing checkpoint",
			checkpointContent: "",
			expectedError:     true,
			expectedPodAllocs: nil,
		},
		{
			name: "Restore empty entry",
			checkpointContent: `{
				"policyName": "none",
				"defaultCPUSet": "4-6",
				"entries": {},
				"checksum": 354655845
			}`,
			expectedError:     false,
			expectedPodAllocs: nil,
		},
		{
			name:              "Restore checkpoint with invalid JSON",
			checkpointContent: `{`,
			expectedError:     true,
			expectedPodAllocs: nil,
		},
		{
			name: "Restore checkpoint with normal assignment entry",
			checkpointContent: `{
				"policyName": "none",
				"defaultCPUSet": "1-3",
				"entries": {
					"pod": {
						"container1": "1-2",
						"container2": "2-3"
					}
				},
				"checksum": 962272150
			}`,
			expectedError: false,
			expectedPodAllocs: []extension.PodCPUAlloc{
				{
					UID:              "pod",
					CPUSet:           "1-3",
					ManagedByKubelet: true,
				},
			},
		},
		{
			name: "Filter Managed Pods",
			checkpointContent: `
				{
				    "policyName": "none",
				    "defaultCPUSet": "1-8",
				    "entries": {
				        "pod": {
				            "container1": "1-2",
				            "container2": "2-3"
				        },
				        "LSPod": {
				            "container1": "3-4"   
				        },
				        "BEPod": {
				            "container1": "4-5"   
				        },
				        "LSRPod": {
				            "container1": "5-6"   
				        },
				        "LSEPod": {
				            "container1": "6-7"   
				        }
				    },
				    "checksum": 962272150
				}`,
			podMap: map[string]*statesinformer.PodMeta{
				"pod": {
					Pod: &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: "default",
							Name:      "test-pod",
							UID:       types.UID("pod"),
						},
					},
				},
				"LSPod": {
					Pod: &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: "default",
							Name:      "test-ls-pod",
							UID:       types.UID("LSPod"),
							Labels: map[string]string{
								extension.LabelPodQoS: string(extension.QoSLS),
							},
							Annotations: map[string]string{
								extension.AnnotationResourceStatus: `{"cpuset": "3-4"}`,
							},
						},
					},
				},
				"BEPod": {
					Pod: &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: "default",
							Name:      "test-be-pod",
							UID:       types.UID("BEPod"),
							Labels: map[string]string{
								extension.LabelPodQoS: string(extension.QoSBE),
							},
						},
					},
				},
				"LSRPod": {
					Pod: &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: "default",
							Name:      "test-lsr-pod",
							UID:       types.UID("LSRPod"),
							Labels: map[string]string{
								extension.LabelPodQoS: string(extension.QoSLSR),
							},
							Annotations: map[string]string{
								extension.AnnotationResourceStatus: `{"cpuset": "4-5"}`,
							},
						},
					},
				},
				"LSEPod": {
					Pod: &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: "default",
							Name:      "test-lse-pod",
							UID:       types.UID("LSEPod"),
							Labels: map[string]string{
								extension.LabelPodQoS: string(extension.QoSLSE),
							},
							Annotations: map[string]string{
								extension.AnnotationResourceStatus: `{"cpuset": "5-6"}`,
							},
						},
					},
				},
			},
			expectedError: false,
			expectedPodAllocs: []extension.PodCPUAlloc{
				{
					Namespace:        "default",
					Name:             "test-pod",
					UID:              "pod",
					CPUSet:           "1-3",
					ManagedByKubelet: true,
				},
			},
		},
	}
	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			s := &nodeTopoInformer{
				podsInformer: &podsInformer{
					podMap: tt.podMap,
				},
			}
			podAllocs, err := s.calGuaranteedCpu(map[int32]*extension.CPUInfo{}, tt.checkpointContent)
			assert.Equal(t, tt.expectedError, err != nil)
			assert.Equal(t, tt.expectedPodAllocs, podAllocs)
		})
	}
}

func Test_reportNodeTopology(t *testing.T) {
	ctl := gomock.NewController(t)
	defer ctl.Finish()

	client := faketopologyclientset.NewSimpleClientset()
	testNodeTemp := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Annotations: map[string]string{},
		},
	}

	mockMetricCache := mock_metriccache.NewMockMetricCache(ctl)
	mockNodeCPUInfo := metriccache.NodeCPUInfo{
		ProcessorInfos: []koordletutil.ProcessorInfo{
			{CPUID: 0, CoreID: 0, NodeID: 0, SocketID: 0},
			{CPUID: 1, CoreID: 0, NodeID: 0, SocketID: 0},
			{CPUID: 2, CoreID: 1, NodeID: 0, SocketID: 0},
			{CPUID: 3, CoreID: 1, NodeID: 0, SocketID: 0},
			{CPUID: 4, CoreID: 2, NodeID: 1, SocketID: 1},
			{CPUID: 5, CoreID: 2, NodeID: 1, SocketID: 1},
			{CPUID: 6, CoreID: 3, NodeID: 1, SocketID: 1},
			{CPUID: 7, CoreID: 3, NodeID: 1, SocketID: 1},
		},
		TotalInfo: koordletutil.CPUTotalInfo{
			NumberCPUs: 8,
			CoreToCPU: map[int32][]koordletutil.ProcessorInfo{
				0: {
					{CPUID: 0, CoreID: 0, NodeID: 0, SocketID: 0},
					{CPUID: 1, CoreID: 0, NodeID: 0, SocketID: 0},
				},
				1: {
					{CPUID: 2, CoreID: 1, NodeID: 0, SocketID: 0},
					{CPUID: 3, CoreID: 1, NodeID: 0, SocketID: 0},
				},
				2: {
					{CPUID: 4, CoreID: 2, NodeID: 1, SocketID: 1},
					{CPUID: 5, CoreID: 2, NodeID: 1, SocketID: 1},
				},
				3: {
					{CPUID: 6, CoreID: 3, NodeID: 1, SocketID: 1},
					{CPUID: 7, CoreID: 3, NodeID: 1, SocketID: 1},
				},
			},
			NodeToCPU: map[int32][]koordletutil.ProcessorInfo{
				0: {
					{CPUID: 0, CoreID: 0, NodeID: 0, SocketID: 0},
					{CPUID: 1, CoreID: 0, NodeID: 0, SocketID: 0},
					{CPUID: 2, CoreID: 1, NodeID: 0, SocketID: 0},
					{CPUID: 3, CoreID: 1, NodeID: 0, SocketID: 0},
				},
				1: {
					{CPUID: 4, CoreID: 2, NodeID: 1, SocketID: 1},
					{CPUID: 5, CoreID: 2, NodeID: 1, SocketID: 1},
					{CPUID: 6, CoreID: 3, NodeID: 1, SocketID: 1},
					{CPUID: 7, CoreID: 3, NodeID: 1, SocketID: 1},
				},
			},
			SocketToCPU: map[int32][]koordletutil.ProcessorInfo{
				0: {
					{CPUID: 0, CoreID: 0, NodeID: 0, SocketID: 0},
					{CPUID: 1, CoreID: 0, NodeID: 0, SocketID: 0},
					{CPUID: 2, CoreID: 1, NodeID: 0, SocketID: 0},
					{CPUID: 3, CoreID: 1, NodeID: 0, SocketID: 0},
				},
				1: {
					{CPUID: 4, CoreID: 2, NodeID: 1, SocketID: 1},
					{CPUID: 5, CoreID: 2, NodeID: 1, SocketID: 1},
					{CPUID: 6, CoreID: 3, NodeID: 1, SocketID: 1},
					{CPUID: 7, CoreID: 3, NodeID: 1, SocketID: 1},
				},
			},
		},
	}
	testMemInfo0 := &koordletutil.MemInfo{
		MemTotal: 263432804, MemFree: 254391744, MemAvailable: 256703236,
		Buffers: 958096, Cached: 0, SwapCached: 0,
		Active: 2786012, Inactive: 2223752, ActiveAnon: 289488,
		InactiveAnon: 1300, ActiveFile: 2496524, InactiveFile: 2222452,
		Unevictable: 0, Mlocked: 0, SwapTotal: 0,
		SwapFree: 0, Dirty: 624, Writeback: 0,
		AnonPages: 281748, Mapped: 495936, Shmem: 2340,
		Slab: 1097040, SReclaimable: 445164, SUnreclaim: 651876,
		KernelStack: 20944, PageTables: 7896, NFS_Unstable: 0,
		Bounce: 0, WritebackTmp: 0, AnonHugePages: 38912,
		HugePages_Total: 0, HugePages_Free: 0, HugePages_Rsvd: 0,
		HugePages_Surp: 0,
	}
	testMemInfo1 := &koordletutil.MemInfo{
		MemTotal: 263432000, MemFree: 254391744, MemAvailable: 256703236,
		Buffers: 958096, Cached: 0, SwapCached: 0,
		Active: 2786012, Inactive: 2223752, ActiveAnon: 289488,
		InactiveAnon: 1300, ActiveFile: 2496524, InactiveFile: 2222452,
		Unevictable: 0, Mlocked: 0, SwapTotal: 0,
		SwapFree: 0, Dirty: 624, Writeback: 0,
		AnonPages: 281748, Mapped: 495936, Shmem: 2340,
		Slab: 1097040, SReclaimable: 445164, SUnreclaim: 651876,
		KernelStack: 20944, PageTables: 7896, NFS_Unstable: 0,
		Bounce: 0, WritebackTmp: 0, AnonHugePages: 38912,
		HugePages_Total: 0, HugePages_Free: 0, HugePages_Rsvd: 0,
		HugePages_Surp: 0,
	}
	mockNodeNUMAInfo := &koordletutil.NodeNUMAInfo{
		NUMAInfos: []koordletutil.NUMAInfo{
			{
				NUMANodeID: 0,
				MemInfo:    testMemInfo0,
			},
			{
				NUMANodeID: 1,
				MemInfo:    testMemInfo1,
			},
		},
		MemInfoMap: map[int32]*koordletutil.MemInfo{
			0: testMemInfo0,
			1: testMemInfo1,
		},
	}

	mockPodMeta := map[string]*statesinformer.PodMeta{
		"pod1": {
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: "ns1",
					Annotations: map[string]string{
						extension.AnnotationResourceStatus: `{"cpuset": "4-5" }`,
					},
				},
			},
		},
		"pod2": {
			Pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod2",
					Namespace: "ns2",
					Annotations: map[string]string{
						extension.AnnotationResourceStatus: `{"cpuset": "3" }`,
					},
				},
			},
		},
	}
	mockMetricCache.EXPECT().Get(metriccache.NodeCPUInfoKey).Return(&mockNodeCPUInfo, true).AnyTimes()
	mockMetricCache.EXPECT().Get(metriccache.NodeNUMAInfoKey).Return(mockNodeNUMAInfo, true).AnyTimes()

	expectedCPUSharedPool := `[{"socket":0,"node":0,"cpuset":"0-2"},{"socket":1,"node":1,"cpuset":"6-7"}]`
	expectedCPUTopology := `{"detail":[{"id":0,"core":0,"socket":0,"node":0},{"id":1,"core":0,"socket":0,"node":0},{"id":2,"core":1,"socket":0,"node":0},{"id":3,"core":1,"socket":0,"node":0},{"id":4,"core":2,"socket":1,"node":1},{"id":5,"core":2,"socket":1,"node":1},{"id":6,"core":3,"socket":1,"node":1},{"id":7,"core":3,"socket":1,"node":1}]}`

	tests := []struct {
		name                            string
		config                          *Config
		kubeletStub                     KubeletStub
		disableCreateTopologyCRD        bool
		nodeReserved                    *extension.NodeReservation
		systemQOSRes                    *extension.SystemQOSResource
		expectedKubeletCPUManagerPolicy extension.KubeletCPUManagerPolicy
		expectedCPUSharedPool           string
		expectedCPUTopology             string
		expectedNodeReservation         string
		expectedSystemQOS               string
	}{
		{
			name:   "report topology",
			config: NewDefaultConfig(),
			kubeletStub: &testKubeletStub{
				config: &kubeletconfiginternal.KubeletConfiguration{
					CPUManagerPolicy: "static",
					KubeReserved: map[string]string{
						"cpu": "2000m",
					},
				},
			},
			expectedKubeletCPUManagerPolicy: extension.KubeletCPUManagerPolicy{
				Policy:       "static",
				ReservedCPUs: "0-1",
			},
			expectedCPUSharedPool:   expectedCPUSharedPool,
			expectedCPUTopology:     expectedCPUTopology,
			expectedNodeReservation: "{}",
			expectedSystemQOS:       "{}",
		},
		{
			name:   "report node topo with reserved and system qos specified",
			config: NewDefaultConfig(),
			kubeletStub: &testKubeletStub{
				config: &kubeletconfiginternal.KubeletConfiguration{
					CPUManagerPolicy: "static",
					KubeReserved: map[string]string{
						"cpu": "2000m",
					},
				},
			},
			disableCreateTopologyCRD: false,
			nodeReserved: &extension.NodeReservation{
				ReservedCPUs: "1-2",
			},
			systemQOSRes: &extension.SystemQOSResource{
				CPUSet: "7",
			},
			expectedKubeletCPUManagerPolicy: extension.KubeletCPUManagerPolicy{
				Policy:       "static",
				ReservedCPUs: "0-1",
			},
			expectedCPUSharedPool:   `[{"socket":0,"node":0,"cpuset":"0"},{"socket":1,"node":1,"cpuset":"6"}]`,
			expectedCPUTopology:     expectedCPUTopology,
			expectedNodeReservation: `{"reservedCPUs":"1-2"}`,
			expectedSystemQOS:       `{"cpuset":"7"}`,
		},
		{
			name: "disable query topology",
			config: &Config{
				DisableQueryKubeletConfig: true,
			},
			kubeletStub: &testKubeletStub{
				config: &kubeletconfiginternal.KubeletConfiguration{
					CPUManagerPolicy: "static",
					KubeReserved: map[string]string{
						"cpu": "2000m",
					},
				},
			},
			expectedKubeletCPUManagerPolicy: extension.KubeletCPUManagerPolicy{
				Policy:       "",
				ReservedCPUs: "",
			},
			expectedCPUSharedPool:   expectedCPUSharedPool,
			expectedCPUTopology:     expectedCPUTopology,
			expectedNodeReservation: "{}",
			expectedSystemQOS:       "{}",
		},
		{
			name:                     "disable report topology",
			disableCreateTopologyCRD: true,
			config:                   NewDefaultConfig(),
			kubeletStub: &testKubeletStub{
				config: &kubeletconfiginternal.KubeletConfiguration{
					CPUManagerPolicy: "static",
					KubeReserved: map[string]string{
						"cpu": "2000m",
					},
				},
			},
			expectedKubeletCPUManagerPolicy: extension.KubeletCPUManagerPolicy{
				Policy:       "static",
				ReservedCPUs: "0-1",
			},
			expectedCPUSharedPool:   expectedCPUSharedPool,
			expectedCPUTopology:     expectedCPUTopology,
			expectedNodeReservation: "{}",
			expectedSystemQOS:       "{}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// prepare feature map
			enabled := features.DefaultKoordletFeatureGate.Enabled(features.NodeTopologyReport)
			testFeatureGates := map[string]bool{string(features.NodeTopologyReport): !tt.disableCreateTopologyCRD}
			err := features.DefaultMutableKoordletFeatureGate.SetFromMap(testFeatureGates)
			assert.NoError(t, err)
			defer func() {
				testFeatureGates[string(features.NodeTopologyReport)] = enabled
				err = features.DefaultMutableKoordletFeatureGate.SetFromMap(testFeatureGates)
				assert.NoError(t, err)
			}()

			testNode := testNodeTemp.DeepCopy()
			if tt.nodeReserved != nil {
				testNode.Annotations[extension.AnnotationNodeReservation] = util.DumpJSON(tt.nodeReserved)
			}
			if tt.systemQOSRes != nil {
				testNode.Annotations[extension.AnnotationNodeSystemQOSResource] = util.DumpJSON(tt.systemQOSRes)
			}

			r := &nodeTopoInformer{
				config:         tt.config,
				kubelet:        tt.kubeletStub,
				topologyClient: client,
				metricCache:    mockMetricCache,
				nodeResourceTopologyLister: &fakeNodeResourceTopologyLister{
					nodeResourceTopologys: &topologyv1alpha1.NodeResourceTopology{
						ObjectMeta: metav1.ObjectMeta{
							Name: "test",
						},
					},
				},
				podsInformer: &podsInformer{
					podMap: mockPodMeta,
				},
				nodeInformer: &nodeInformer{
					node: testNode,
				},
				callbackRunner: NewCallbackRunner(),
			}

			topologyName := testNode.Name
			_ = client.TopologyV1alpha1().NodeResourceTopologies().Delete(context.TODO(), topologyName, metav1.DeleteOptions{})
			if !tt.disableCreateTopologyCRD {
				topologyTest := newNodeTopo(testNode)
				_, err = client.TopologyV1alpha1().NodeResourceTopologies().Create(context.TODO(), topologyTest, metav1.CreateOptions{})
			}
			r.reportNodeTopology()

			var topology *topologyv1alpha1.NodeResourceTopology
			if tt.disableCreateTopologyCRD {
				topology = r.GetNodeTopo()
				_, err = client.TopologyV1alpha1().NodeResourceTopologies().Get(context.TODO(), topologyName, metav1.GetOptions{})
				assert.True(t, errors.IsNotFound(err))
			} else {
				topology, err = client.TopologyV1alpha1().NodeResourceTopologies().Get(context.TODO(), topologyName, metav1.GetOptions{})
				assert.NoError(t, err)
			}

			var kubeletCPUManagerPolicy extension.KubeletCPUManagerPolicy
			err = json.Unmarshal([]byte(topology.Annotations[extension.AnnotationKubeletCPUManagerPolicy]), &kubeletCPUManagerPolicy)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedKubeletCPUManagerPolicy, kubeletCPUManagerPolicy)
			assert.Equal(t, tt.expectedCPUSharedPool, topology.Annotations[extension.AnnotationNodeCPUSharedPools])
			assert.Equal(t, tt.expectedCPUTopology, topology.Annotations[extension.AnnotationNodeCPUTopology])
			assert.Equal(t, tt.expectedNodeReservation, topology.Annotations[extension.AnnotationNodeReservation])
			assert.Equal(t, tt.expectedSystemQOS, topology.Annotations[extension.AnnotationNodeSystemQOSResource])
		})
	}
}

func Test_isSyncNeeded(t *testing.T) {
	type args struct {
		isOldEmpty bool
		oldtopo    map[string]string
		newtopo    map[string]string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "old is nil",
			args: args{
				isOldEmpty: true,
				newtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
					"node.koordinator.sh/pod-cpu-allocs":        "{\"Namespace\":\"default1\",\"Name\":\"test-pod\",\"UID\":\"pod\",\"CPUSet\":\"1-3\",\"ManagedByKubelet\": \"true\"}",
				},
			},
			want: true,
		},
		{
			name: "annotation is new",
			args: args{
				oldtopo: nil,
				newtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
					"node.koordinator.sh/pod-cpu-allocs":        "{\"Namespace\":\"default1\",\"Name\":\"test-pod\",\"UID\":\"pod\",\"CPUSet\":\"1-3\",\"ManagedByKubelet\": \"true\"}",
				},
			},
			want: true,
		},
		{
			name: "same json with different map order in cpu share pool",
			args: args{
				oldtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"cpuset\":\"0-25,52-77\",\"socket\":0,\"node\":0},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
				},
				newtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
				},
			},
			want: false,
		},
		{
			name: "diff json on pod-cpu-allocs",
			args: args{
				oldtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
					"node.koordinator.sh/pod-cpu-allocs":        "{\"Namespace\":\"default\",\"Name\":\"test-pod\",\"UID\":\"pod\",\"CPUSet\":\"1-3\",\"ManagedByKubelet\": \"true\"}",
				},
				newtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
					"node.koordinator.sh/pod-cpu-allocs":        "{\"Namespace\":\"default1\",\"Name\":\"test-pod\",\"UID\":\"pod\",\"CPUSet\":\"1-3\",\"ManagedByKubelet\": \"true\"}",
				},
			},
			want: true,
		},
		{
			name: "some are both not exist in old and new",
			args: args{
				oldtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
				},
				newtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
				},
			},
			want: false,
		},
		{
			name: "part are not exist in old",
			args: args{
				oldtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
				},
				newtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
					"node.koordinator.sh/pod-cpu-allocs":        "{\"Namespace\":\"default\",\"Name\":\"test-pod\",\"UID\":\"pod\",\"CPUSet\":\"1-3\",\"ManagedByKubelet\": \"true\"}",
				},
			},
			want: true,
		},
		{
			name: "part are not exist in new",
			args: args{
				oldtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
					"node.koordinator.sh/pod-cpu-allocs":        "{\"Namespace\":\"default\",\"Name\":\"test-pod\",\"UID\":\"pod\",\"CPUSet\":\"1-3\",\"ManagedByKubelet\": \"true\"}",
				},
				newtopo: map[string]string{
					"kubelet.koordinator.sh/cpu-manager-policy": "{\"policy\":\"none\"}",
					"node.koordinator.sh/cpu-shared-pools":      "[{\"socket\":0,\"node\":0,\"cpuset\":\"0-25,52-77\"},{\"socket\":1,\"node\":1,\"cpuset\":\"26-51,78-103\"}]",
					"node.koordinator.sh/cpu-topology":          "{\"detail\":[{\"id\":0,\"core\":0,\"socket\":0,\"node\":0},{\"id\":52,\"core\":0,\"socket\":0,\"node\":0},{\"id\":1,\"core\":1,\"socket\":0,\"node\":0}]}",
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newNRT := &topologyv1alpha1.NodeResourceTopology{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.args.newtopo,
				},
			}
			var oldNRT *topologyv1alpha1.NodeResourceTopology
			if !tt.args.isOldEmpty {
				oldNRT = &topologyv1alpha1.NodeResourceTopology{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: tt.args.oldtopo,
					},
				}
			}

			got := isSyncNeeded(oldNRT, newNRT, "test-node")
			assert.Equalf(t, tt.want, got, "isSyncNeeded(%v, %v)", tt.args.oldtopo, tt.args.newtopo)
		})
	}
}

func Test_getNodeReserved(t *testing.T) {
	fakeTopo := topology.CPUTopology{
		NumCPUs:    12,
		NumSockets: 2,
		NumCores:   6,
		CPUDetails: map[int]topology.CPUInfo{
			0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
			1:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
			2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
			3:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
			4:  {CoreID: 4, SocketID: 0, NUMANodeID: 0},
			5:  {CoreID: 5, SocketID: 1, NUMANodeID: 1},
			6:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
			7:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
			8:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
			9:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
			10: {CoreID: 4, SocketID: 0, NUMANodeID: 0},
			11: {CoreID: 5, SocketID: 1, NUMANodeID: 1},
		},
	}
	type args struct {
		anno map[string]string
	}
	tests := []struct {
		name string
		args args
		want extension.NodeReservation
	}{
		{
			name: "node.annotation is nil",
			args: args{},
			want: extension.NodeReservation{},
		},
		{
			name: "node.annotation not nil but nothing reserved",
			args: args{
				map[string]string{
					"k": "v",
				},
			},
			want: extension.NodeReservation{},
		},
		{
			name: "node.annotation not nil but without cpu reserved",
			args: args{
				map[string]string{
					extension.AnnotationNodeReservation: util.GetNodeAnnoReservedJson(extension.NodeReservation{}),
				},
			},
			want: extension.NodeReservation{},
		},
		{
			name: "reserve cpu only by quantity",
			args: args{
				map[string]string{
					extension.AnnotationNodeReservation: util.GetNodeAnnoReservedJson(extension.NodeReservation{
						Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					}),
				},
			},
			want: extension.NodeReservation{ReservedCPUs: "0,6"},
		},
		{
			name: "reserve cpu only by quantity but value not integer",
			args: args{
				map[string]string{
					extension.AnnotationNodeReservation: util.GetNodeAnnoReservedJson(extension.NodeReservation{
						Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2.5")},
					}),
				},
			},
			want: extension.NodeReservation{ReservedCPUs: "0,2,6"},
		},
		{
			name: "reserve cpu only by quantity but value is negative",
			args: args{
				map[string]string{
					extension.AnnotationNodeReservation: util.GetNodeAnnoReservedJson(extension.NodeReservation{
						Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("-2")},
					}),
				},
			},
			want: extension.NodeReservation{},
		},
		{
			name: "reserve cpu only by specific cpus",
			args: args{
				map[string]string{
					extension.AnnotationNodeReservation: util.GetNodeAnnoReservedJson(extension.NodeReservation{
						ReservedCPUs: "0-1",
					}),
				},
			},
			want: extension.NodeReservation{ReservedCPUs: "0-1"},
		},
		{
			name: "reserve cpu only by specific cpus but core id is unavailable",
			args: args{
				map[string]string{
					extension.AnnotationNodeReservation: util.GetNodeAnnoReservedJson(extension.NodeReservation{
						ReservedCPUs: "-1",
					}),
				},
			},
			want: extension.NodeReservation{},
		},
		{
			name: "reserve cpu by specific cpus and quantity",
			args: args{
				map[string]string{
					extension.AnnotationNodeReservation: util.GetNodeAnnoReservedJson(extension.NodeReservation{
						Resources:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10")},
						ReservedCPUs: "0-1",
					}),
				},
			},
			want: extension.NodeReservation{ReservedCPUs: "0-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getNodeReserved(&fakeTopo, tt.args.anno); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getNodeReserved() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_removeSystemQOSCPUs(t *testing.T) {
	originCPUSharePool := []extension.CPUSharedPool{
		{
			Socket: 0,
			Node:   0,
			CPUSet: "0-7",
		},
		{
			Socket: 1,
			Node:   0,
			CPUSet: "8-15",
		},
	}
	type args struct {
		cpuSharePools []extension.CPUSharedPool
		sysQOSRes     *extension.SystemQOSResource
	}
	tests := []struct {
		name string
		args args
		want []extension.CPUSharedPool
	}{
		{
			name: "system qos res is nil",
			args: args{
				cpuSharePools: originCPUSharePool,
				sysQOSRes:     nil,
			},
			want: originCPUSharePool,
		},
		{
			name: "system qos res is empty cpuset",
			args: args{
				cpuSharePools: originCPUSharePool,
				sysQOSRes: &extension.SystemQOSResource{
					CPUSet: "",
				},
			},
			want: originCPUSharePool,
		},
		{
			name: "system qos res is not exclusive",
			args: args{
				cpuSharePools: originCPUSharePool,
				sysQOSRes: &extension.SystemQOSResource{
					CPUSet:          "0-3",
					CPUSetExclusive: pointer.Bool(false),
				},
			},
			want: originCPUSharePool,
		},
		{
			name: "system qos with bad cpuset fmt",
			args: args{
				cpuSharePools: originCPUSharePool,
				sysQOSRes: &extension.SystemQOSResource{
					CPUSet: "0b",
				},
			},
			want: originCPUSharePool,
		},
		{
			name: "exclude cpuset from share pool",
			args: args{
				cpuSharePools: originCPUSharePool,
				sysQOSRes: &extension.SystemQOSResource{
					CPUSet: "0-3",
				},
			},
			want: []extension.CPUSharedPool{
				{
					Socket: 0,
					Node:   0,
					CPUSet: "4-7",
				},
				{
					Socket: 1,
					Node:   0,
					CPUSet: "8-15",
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := removeSystemQOSCPUs(tt.args.cpuSharePools, tt.args.sysQOSRes); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("removeSystemQOSCPUs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_getTopologyPolicy(t *testing.T) {
	type args struct {
		topologyManagerPolicy string
		topologyManagerScope  string
	}
	tests := []struct {
		name string
		args args
		want topologyv1alpha1.TopologyManagerPolicy
	}{
		{
			name: "get None policy by default",
			want: topologyv1alpha1.None,
		},
		{
			name: "get None policy by default 1",
			args: args{
				topologyManagerScope: kubeletconfiginternal.ContainerTopologyManagerScope,
			},
			want: topologyv1alpha1.None,
		},
		{
			name: "get None policy by default 2",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.BestEffortTopologyManagerPolicy,
			},
			want: topologyv1alpha1.None,
		},
		{
			name: "get container single numa policy",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.SingleNumaNodeTopologyManagerPolicy,
				topologyManagerScope:  kubeletconfiginternal.ContainerTopologyManagerScope,
			},
			want: topologyv1alpha1.SingleNUMANodeContainerLevel,
		},
		{
			name: "get container restricted policy",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.RestrictedTopologyManagerPolicy,
				topologyManagerScope:  kubeletconfiginternal.ContainerTopologyManagerScope,
			},
			want: topologyv1alpha1.RestrictedContainerLevel,
		},
		{
			name: "get container besteffort policy",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.BestEffortTopologyManagerPolicy,
				topologyManagerScope:  kubeletconfiginternal.ContainerTopologyManagerScope,
			},
			want: topologyv1alpha1.BestEffortContainerLevel,
		},
		{
			name: "get container none policy",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.NoneTopologyManagerPolicy,
				topologyManagerScope:  kubeletconfiginternal.ContainerTopologyManagerScope,
			},
			want: topologyv1alpha1.None,
		},
		{
			name: "get pod single numa policy",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.SingleNumaNodeTopologyManagerPolicy,
				topologyManagerScope:  kubeletconfiginternal.PodTopologyManagerScope,
			},
			want: topologyv1alpha1.SingleNUMANodePodLevel,
		},
		{
			name: "get pod restricted policy",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.RestrictedTopologyManagerPolicy,
				topologyManagerScope:  kubeletconfiginternal.PodTopologyManagerScope,
			},
			want: topologyv1alpha1.RestrictedPodLevel,
		},
		{
			name: "get pod besteffort policy",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.BestEffortTopologyManagerPolicy,
				topologyManagerScope:  kubeletconfiginternal.PodTopologyManagerScope,
			},
			want: topologyv1alpha1.BestEffortPodLevel,
		},
		{
			name: "get pod none policy",
			args: args{
				topologyManagerPolicy: kubeletconfiginternal.NoneTopologyManagerPolicy,
				topologyManagerScope:  kubeletconfiginternal.PodTopologyManagerScope,
			},
			want: topologyv1alpha1.None,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getTopologyPolicy(tt.args.topologyManagerPolicy, tt.args.topologyManagerScope)
			assert.Equal(t, tt.want, got)
		})
	}
}
