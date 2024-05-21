package reconciliation

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	apiv1 "github.com/Azure/eno/api/v1"
	"github.com/Azure/eno/internal/controllers/aggregation"
	testv1 "github.com/Azure/eno/internal/controllers/reconciliation/fixtures/v1"
	"github.com/Azure/eno/internal/controllers/rollout"
	"github.com/Azure/eno/internal/controllers/synthesis"
	"github.com/Azure/eno/internal/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReadinessGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1.SchemeBuilder.AddToScheme(scheme)
	testv1.SchemeBuilder.AddToScheme(scheme)

	ctx := testutil.NewContext(t)
	mgr := testutil.NewManager(t)
	upstream := mgr.GetClient()

	// Register supporting controllers
	require.NoError(t, rollout.NewController(mgr.Manager, time.Millisecond))
	require.NoError(t, synthesis.NewStatusController(mgr.Manager))
	require.NoError(t, aggregation.NewSliceController(mgr.Manager))
	require.NoError(t, synthesis.NewPodLifecycleController(mgr.Manager, defaultConf))
	require.NoError(t, synthesis.NewSliceCleanupController(mgr.Manager))
	require.NoError(t, synthesis.NewExecController(mgr.Manager, defaultConf, &testutil.ExecConn{Hook: func(s *apiv1.Synthesizer) []client.Object {
		obj := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-obj-0",
				Namespace: "default",
			},
			Data: map[string]string{"image": s.Spec.Image},
		}

		gvks, _, err := scheme.ObjectKinds(obj)
		require.NoError(t, err)
		obj.GetObjectKind().SetGroupVersionKind(gvks[0])

		obj1 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-obj-1",
				Namespace:   "default",
				Annotations: map[string]string{"eno.azure.io/readiness-group": "2"},
			},
			Data: map[string]string{"image": s.Spec.Image},
		}

		gvks, _, err = scheme.ObjectKinds(obj1)
		require.NoError(t, err)
		obj1.GetObjectKind().SetGroupVersionKind(gvks[0])

		obj2 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-obj-2",
				Namespace:   "default",
				Annotations: map[string]string{"eno.azure.io/readiness-group": "4"},
			},
			Data: map[string]string{"image": s.Spec.Image},
		}

		gvks, _, err = scheme.ObjectKinds(obj2)
		require.NoError(t, err)
		obj2.GetObjectKind().SetGroupVersionKind(gvks[0])

		return []client.Object{obj, obj2, obj1}
	}}))

	// Test subject
	setupTestSubject(t, mgr)
	mgr.Start(t)

	syn := &apiv1.Synthesizer{}
	syn.Name = "test-syn"
	syn.Spec.Image = "create"
	require.NoError(t, upstream.Create(ctx, syn))

	comp := &apiv1.Composition{}
	comp.Name = "test-comp"
	comp.Namespace = "default"
	comp.Spec.Synthesizer.Name = syn.Name
	require.NoError(t, upstream.Create(ctx, comp))

	// Wait for reconciliation
	testutil.Eventually(t, func() bool {
		err := upstream.Get(ctx, client.ObjectKeyFromObject(comp), comp)
		return err == nil && comp.Status.CurrentSynthesis != nil && comp.Status.CurrentSynthesis.Reconciled != nil && comp.Status.CurrentSynthesis.ObservedSynthesizerGeneration == syn.Generation
	})

	// Prove resources were created in the expected order
	// Technically resource version is an opaque string, realistically it won't be changing
	// any time soon so it's safe to use here and less flaky than the creation timestamp
	assertOrder := func() {
		resourceVersions := []int{}
		for i := 0; i < 2; i++ {
			cm := &corev1.ConfigMap{}
			cm.Name = fmt.Sprintf("test-obj-%d", i)
			cm.Namespace = "default"
			err := mgr.DownstreamClient.Get(ctx, client.ObjectKeyFromObject(cm), cm)
			require.NoError(t, err)

			rv, _ := strconv.Atoi(cm.ResourceVersion)
			resourceVersions = append(resourceVersions, rv)
		}
		if !slices.IsSorted(resourceVersions) { // ascending
			t.Errorf("expected resource versions to be sorted: %+d", resourceVersions)
		}
	}
	assertOrder()

	// Updates should also be ordered
	err := retry.RetryOnConflict(testutil.Backoff, func() error {
		upstream.Get(ctx, client.ObjectKeyFromObject(syn), syn)
		syn.Spec.Image = "updated"
		return upstream.Update(ctx, syn)
	})
	require.NoError(t, err)

	testutil.Eventually(t, func() bool {
		err := upstream.Get(ctx, client.ObjectKeyFromObject(comp), comp)
		return err == nil && comp.Status.CurrentSynthesis != nil && comp.Status.CurrentSynthesis.Reconciled != nil && comp.Status.CurrentSynthesis.ObservedSynthesizerGeneration == syn.Generation
	})
	assertOrder()

	// Deletes should not be ordered
	require.NoError(t, upstream.Delete(ctx, comp))
	testutil.Eventually(t, func() bool {
		err := upstream.Get(ctx, client.ObjectKeyFromObject(comp), comp)
		return errors.IsNotFound(err)
	})
}