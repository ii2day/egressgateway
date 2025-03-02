// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package endpoint

import (
	"context"
	"errors"
	"fmt"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/spidernet-io/egressgateway/pkg/config"
	"github.com/spidernet-io/egressgateway/pkg/k8s/apis/v1beta1"
	"github.com/spidernet-io/egressgateway/pkg/logger"
	"github.com/spidernet-io/egressgateway/pkg/schema"
)

type TestCaseEPS struct {
	initialObjects []client.Object
	reqs           []TestEgressGatewayPolicyReq
	config         *config.Config
}

type TestEgressGatewayPolicyReq struct {
	nn         types.NamespacedName
	expErr     bool
	expRequeue bool
}

func TestReconcilerEgressEndpointSlice(t *testing.T) {
	cases := map[string]TestCaseEPS{
		"caseAddEgressGatewayPolicy": caseAddPolicy(),
		"caseUpdatePod":              caseUpdatePod(),
		"caseDeletePod":              caseDeletePod(),
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			builder := fake.NewClientBuilder()
			builder.WithScheme(schema.GetScheme())
			builder.WithObjects(c.initialObjects...)
			cli := builder.Build()
			reconciler := endpointReconciler{
				client: cli,
				log:    logger.NewLogger(c.config.EnvConfig.Logger),
				config: c.config,
			}

			for _, req := range c.reqs {
				res, err := reconciler.Reconcile(
					context.Background(),
					reconcile.Request{NamespacedName: req.nn},
				)
				if !req.expErr {
					assert.NoError(t, err)
				}
				assert.Equal(t, req.expRequeue, res.Requeue)

				ctx := context.Background()
				policy := new(v1beta1.EgressPolicy)
				err = cli.Get(ctx, req.nn, policy)
				if err != nil {
					t.Fatal(err)
				}

				epList, err := listEndpointSlices(ctx, cli, policy.Namespace, policy.Name)
				if err != nil {
					t.Fatal(err)
				}

				pods, err := listPodsByPolicy(ctx, cli, policy)
				if err != nil {
					t.Fatal(err)
				}

				err = checkPolicyIPsInEpSlice(pods, epList)
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func checkPolicyIPsInEpSlice(pods *corev1.PodList, eps *v1beta1.EgressEndpointSliceList) error {
	if pods == nil || eps == nil {
		return nil
	}
	podMap := make(map[string]struct{})
	epMap := make(map[string]struct{})
	for _, pod := range pods.Items {
		for _, ip := range pod.Status.PodIPs {
			podMap[ip.IP] = struct{}{}
		}
	}

	for _, item := range eps.Items {
		for _, endpoint := range item.Endpoints {
			for _, ip := range endpoint.IPv4 {
				epMap[ip] = struct{}{}
			}
			for _, ip := range endpoint.IPv6 {
				epMap[ip] = struct{}{}
			}
		}
	}

	return compareMaps(podMap, epMap)
}

func compareMaps(podMap, epMap map[string]struct{}) error {
	var missingPods, missingEps []string

	for k := range podMap {
		if _, ok := epMap[k]; !ok {
			missingEps = append(missingEps, k)
		}
	}

	for k := range epMap {
		if _, ok := podMap[k]; !ok {
			missingPods = append(missingPods, k)
		}
	}

	if len(missingPods) > 0 || len(missingEps) > 0 {
		var msg string
		if len(missingPods) > 0 {
			msg += fmt.Sprintf("missing endpoints for pods: %v\n", missingPods)
		}
		if len(missingEps) > 0 {
			msg += fmt.Sprintf("missing pods ip for endpoints: %v\n", missingEps)
		}
		return errors.New(msg)
	}

	return nil
}

func caseAddPolicy() TestCaseEPS {
	labels := map[string]string{"app": "nginx1"}
	initialObjects := []client.Object{
		&v1beta1.EgressPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "policy1",
				Namespace: "default",
			},
			Spec: v1beta1.EgressPolicySpec{
				EgressGatewayName: "",
				EgressIP:          v1beta1.EgressIP{},
				AppliedTo: v1beta1.AppliedTo{
					PodSelector: &metav1.LabelSelector{MatchLabels: labels},
				},
				DestSubnet: nil,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.1"},
					{IP: "10.6.0.2"},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.3"},
					{IP: "10.6.0.4"},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod3",
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.5"},
					{IP: "10.6.0.6"},
				},
			},
		},
	}
	reqs := []TestEgressGatewayPolicyReq{
		{
			nn:         types.NamespacedName{Namespace: "default", Name: "policy1"},
			expErr:     false,
			expRequeue: false,
		},
	}

	conf := &config.Config{
		FileConfig: config.FileConfig{
			MaxNumberEndpointPerSlice: 2,
		},
	}

	return TestCaseEPS{
		initialObjects: initialObjects,
		reqs:           reqs,
		config:         conf,
	}
}

func caseUpdatePod() TestCaseEPS {
	labels := map[string]string{"app": "nginx1"}
	initialObjects := []client.Object{
		&v1beta1.EgressPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "policy1",
				Namespace: "default",
			},
			Spec: v1beta1.EgressPolicySpec{
				EgressGatewayName: "",
				EgressIP:          v1beta1.EgressIP{},
				AppliedTo: v1beta1.AppliedTo{
					PodSelector: &metav1.LabelSelector{MatchLabels: labels},
				},
				DestSubnet: nil,
			},
		},
		&v1beta1.EgressEndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "s1",
				Namespace: "default",
				Labels: map[string]string{
					v1beta1.LabelPolicyName: "policy1",
				},
			},
			Endpoints: []v1beta1.EgressEndpoint{
				{
					Namespace: "default",
					Pod:       "pod1",
					IPv4: []string{
						"10.6.0.1", "10.6.0.2",
					},
					IPv6: []string{},
					Node: "",
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.1"},
					{IP: "10.6.0.2"},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.3"},
					{IP: "10.6.0.4"},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod3",
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.5"},
					{IP: "10.6.0.6"},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod4",
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.7"},
					{IP: "10.6.0.8"},
				},
			},
		},
	}
	reqs := []TestEgressGatewayPolicyReq{
		{
			nn:         types.NamespacedName{Namespace: "default", Name: "policy1"},
			expErr:     false,
			expRequeue: false,
		},
	}

	conf := &config.Config{
		FileConfig: config.FileConfig{
			MaxNumberEndpointPerSlice: 2,
		},
	}

	return TestCaseEPS{
		initialObjects: initialObjects,
		reqs:           reqs,
		config:         conf,
	}
}

func caseDeletePod() TestCaseEPS {
	labels := map[string]string{"app": "nginx1"}
	initialObjects := []client.Object{
		&v1beta1.EgressPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "policy1",
				Namespace: "default",
			},
			Spec: v1beta1.EgressPolicySpec{
				EgressGatewayName: "",
				EgressIP:          v1beta1.EgressIP{},
				AppliedTo: v1beta1.AppliedTo{
					PodSelector: &metav1.LabelSelector{MatchLabels: labels},
				},
				DestSubnet: nil,
			},
		},
		&v1beta1.EgressEndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "s1",
				Namespace: "default",
				Labels: map[string]string{
					v1beta1.LabelPolicyName: "policy1",
				},
			},
			Endpoints: []v1beta1.EgressEndpoint{
				{
					Namespace: "default",
					Pod:       "pod1",
					IPv4: []string{
						"10.6.0.1",
						"10.6.0.2",
					},
					IPv6: []string{},
					Node: "",
				},
				{
					Namespace: "default",
					Pod:       "pod2",
					IPv4: []string{
						"10.6.0.3",
						"10.6.0.4",
					},
					IPv6: []string{},
					Node: "",
				},
			},
		},
		&v1beta1.EgressEndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "s2",
				Namespace: "default",
				Labels: map[string]string{
					v1beta1.LabelPolicyName: "policy1",
				},
			},
			Endpoints: []v1beta1.EgressEndpoint{
				{
					Namespace: "default",
					Pod:       "pod3",
					IPv4: []string{
						"10.6.0.5",
						"10.6.0.6",
					},
					IPv6: []string{},
					Node: "",
				},
				{
					Namespace: "default",
					Pod:       "pod4",
					IPv4: []string{
						"10.6.0.7",
						"10.6.0.8",
					},
					IPv6: []string{},
					Node: "",
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: "default",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.1"},
					{IP: "10.6.0.2"},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.3"},
					{IP: "10.6.0.4"},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod3",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.5"},
					{IP: "10.6.0.6"},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod4",
				Namespace: "default",
			},
			Spec: corev1.PodSpec{},
			Status: corev1.PodStatus{
				PodIPs: []corev1.PodIP{
					{IP: "10.6.0.7"},
					{IP: "10.6.0.8"},
				},
			},
		},
	}
	reqs := []TestEgressGatewayPolicyReq{
		{
			nn:         types.NamespacedName{Namespace: "default", Name: "policy1"},
			expErr:     false,
			expRequeue: false,
		},
	}

	conf := &config.Config{
		FileConfig: config.FileConfig{
			MaxNumberEndpointPerSlice: 2,
		},
	}

	return TestCaseEPS{
		initialObjects: initialObjects,
		reqs:           reqs,
		config:         conf,
	}
}

func TestPodPredicate(t *testing.T) {
	p := podPredicate{}
	if !p.Create(event.CreateEvent{
		Object: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default",
				Labels: map[string]string{
					"aa": "bb",
				},
			},
			Status: corev1.PodStatus{
				PodIP: "10.6.1.21",
				PodIPs: []corev1.PodIP{
					{IP: "10.6.1.21"},
				},
			},
		},
	}) {
		t.Fatal("got false")
	}

	if !p.Delete(event.DeleteEvent{}) {
		t.Fatal("got false")
	}

	if !p.Update(event.UpdateEvent{
		ObjectOld: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default",
				Labels: map[string]string{
					"aa": "bb",
				},
			},
		},
		ObjectNew: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default",
				Labels: map[string]string{
					"aa": "cc",
				},
			},
		},
	}) {
		t.Fatal("got false")
	}
}

func TestEnqueuePod(t *testing.T) {
	labels := map[string]string{"app": "nginx1"}
	initialObjects := []client.Object{
		&v1beta1.EgressPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p1",
			},
			Spec: v1beta1.EgressPolicySpec{
				AppliedTo: v1beta1.AppliedTo{
					PodSelector: &metav1.LabelSelector{MatchLabels: labels},
				},
			},
		},
	}

	builder := fake.NewClientBuilder()
	builder.WithScheme(schema.GetScheme())
	builder.WithObjects(initialObjects...)
	cli := builder.Build()

	f := enqueuePod(cli)
	ctx := context.Background()

	f(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			Labels:    labels,
		},
	})
}

func TestNewEgressEndpointSliceController(t *testing.T) {
	var initialObjects []client.Object

	builder := fake.NewClientBuilder()
	builder.WithScheme(schema.GetScheme())
	builder.WithObjects(initialObjects...)
	cli := builder.Build()

	mgrOpts := manager.Options{
		Scheme: schema.GetScheme(),
		NewClient: func(config *rest.Config, options client.Options) (client.Client, error) {
			return cli, nil
		},
	}

	cfg := &config.Config{
		KubeConfig: &rest.Config{},
		FileConfig: config.FileConfig{
			MaxNumberEndpointPerSlice: 100,
			IPTables: config.IPTables{
				RefreshIntervalSecond:   90,
				PostWriteIntervalSecond: 1,
				LockTimeoutSecond:       0,
				LockProbeIntervalMillis: 50,
				LockFilePath:            "/run/xtables.lock",
				RestoreSupportsLock:     true,
			},
			Mark: "0x26000000",
			GatewayFailover: config.GatewayFailover{
				Enable:              true,
				TunnelMonitorPeriod: 5,
				TunnelUpdatePeriod:  5,
				EipEvictionTimeout:  15,
			},
		},
	}
	log := logger.NewLogger(cfg.EnvConfig.Logger)
	mgr, err := ctrl.NewManager(cfg.KubeConfig, mgrOpts)
	if err != nil {
		t.Fatal(err)
	}
	err = NewEgressEndpointSliceController(mgr, log, cfg)
	if err != nil {
		t.Fatal(err)
	}
}
