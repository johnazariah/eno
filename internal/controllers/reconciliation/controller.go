package reconciliation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/Azure/eno/api/v1"
	"github.com/Azure/eno/internal/discovery"
	"github.com/Azure/eno/internal/flowcontrol"
	"github.com/Azure/eno/internal/reconstitution"
	"github.com/go-logr/logr"
)

var insecureLogPatch = os.Getenv("INSECURE_LOG_PATCH") == "true"

type Options struct {
	Manager     ctrl.Manager
	Cache       *reconstitution.Cache
	WriteBuffer *flowcontrol.ResourceSliceWriteBuffer
	Downstream  *rest.Config

	DiscoveryRPS float32

	Timeout               time.Duration
	ReadinessPollInterval time.Duration
}

type Controller struct {
	client                client.Client
	writeBuffer           *flowcontrol.ResourceSliceWriteBuffer
	resourceClient        reconstitution.Client
	timeout               time.Duration
	readinessPollInterval time.Duration
	upstreamClient        client.Client
	discovery             *discovery.Cache
}

func New(opts Options) (*Controller, error) {
	upstreamClient, err := client.New(opts.Downstream, client.Options{
		Scheme: runtime.NewScheme(), // empty scheme since we shouldn't rely on compile-time types
	})
	if err != nil {
		return nil, err
	}

	disc, err := discovery.NewCache(opts.Downstream, opts.DiscoveryRPS)
	if err != nil {
		return nil, err
	}

	return &Controller{
		client:                opts.Manager.GetClient(),
		writeBuffer:           opts.WriteBuffer,
		resourceClient:        opts.Cache,
		timeout:               opts.Timeout,
		readinessPollInterval: opts.ReadinessPollInterval,
		upstreamClient:        upstreamClient,
		discovery:             disc,
	}, nil
}

func (c *Controller) Reconcile(ctx context.Context, req *reconstitution.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	comp := &apiv1.Composition{}
	err := c.client.Get(ctx, types.NamespacedName{Name: req.Composition.Name, Namespace: req.Composition.Namespace}, comp)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(fmt.Errorf("getting composition: %w", err))
	}
	logger := logr.FromContextOrDiscard(ctx).WithValues("compositionGeneration", comp.Generation)

	if comp.Status.CurrentSynthesis == nil || comp.Status.CurrentSynthesis.Failed() {
		return ctrl.Result{}, nil // nothing to do
	}
	logger = logger.WithValues("synthesizerName", comp.Spec.Synthesizer.Name,
		"synthesizerGeneration", comp.Status.CurrentSynthesis.ObservedSynthesizerGeneration,
		"synthesisID", comp.Status.GetCurrentSynthesisUUID())
	ctx = logr.NewContext(ctx, logger)

	// Find the current and (optionally) previous desired states in the cache
	synRef := reconstitution.NewSynthesisRef(comp)
	resource, exists := c.resourceClient.Get(ctx, synRef, &req.Resource)
	if !exists {
		// It's possible for the cache to be empty because a manifest for this resource no longer exists at the requested composition generation.
		// Dropping the work item is safe since filling the new version will generate a new queue message.
		logger.V(1).Info("dropping work item because the corresponding synthesis no longer exists in the cache")
		return ctrl.Result{}, nil
	}

	var prev *reconstitution.Resource
	if comp.Status.PreviousSynthesis != nil {
		prevSynRef := reconstitution.NewSynthesisRef(comp)
		prevSynRef.UUID = comp.Status.PreviousSynthesis.UUID
		prev, _ = c.resourceClient.Get(ctx, prevSynRef, &req.Resource)
	}
	logger = logger.WithValues("resourceKind", resource.Ref.Kind, "resourceName", resource.Ref.Name, "resourceNamespace", resource.Ref.Namespace)
	ctx = logr.NewContext(ctx, logger)

	// Keep track of the last reconciliation time and report on it relative to the resource's reconcile interval
	// This is useful for identifying cases where the loop can't keep up
	if resource.ReconcileInterval != nil {
		observation := resource.ObserveReconciliation()
		if observation > 0 {
			delta := observation - resource.ReconcileInterval.Duration
			reconciliationScheduleDelta.Observe(delta.Seconds())
		}
	}

	// CRDs must be reconciled before any CRs that use the type they define.
	// For initial creation just failing and retrying will eventually converge but updates are tricky
	// since unknown properties sent from clients are ignored by apiserver.
	// i.e. ordering is necessary to handle adding a new property and populating it in the same synthesis.
	crdResource, ok := c.resourceClient.GetDefiningCRD(ctx, synRef, resource.GVK.GroupKind())
	if ok {
		slice := &apiv1.ResourceSlice{}
		err = c.client.Get(ctx, crdResource.ManifestRef.Slice, slice)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("getting resource slice: %w", err)
		}
		status := crdResource.FindStatus(slice)
		if status == nil || status.Ready == nil {
			logger.V(1).Info("skipping because the CRD that defines this resource type isn't ready")
			return ctrl.Result{}, nil
		}

		// apiserver doesn't "close the loop" on CRD loading, so there is no way to know
		// when CRDs are actually ready. This normally only takes a couple of milliseconds
		// but we round up to a full second here to be safe.
		if delta := time.Second - time.Since(status.Ready.Time); delta > 0 {
			logger.V(1).Info("deferring until the defining CRD has been ready for 1 second")
			return ctrl.Result{RequeueAfter: delta}, nil
		}
	}

	// Fetch the current resource
	current, hasChanged, err := c.getCurrent(ctx, resource)
	if client.IgnoreNotFound(err) != nil && !isErrMissingNS(err) {
		return ctrl.Result{}, fmt.Errorf("getting current state: %w", err)
	}

	// Evaluate resource readiness
	// - Readiness checks are skipped when this version of the resource's desired state has already become ready
	// - Readiness checks are skipped when the resource hasn't changed since the last check
	// - Readiness defaults to true if no checks are given
	slice := &apiv1.ResourceSlice{}
	err = c.client.Get(ctx, resource.ManifestRef.Slice, slice)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting resource slice: %w", err)
	}
	var ready *metav1.Time
	status := resource.FindStatus(slice)
	if status == nil || status.Ready == nil {
		readiness, ok := resource.ReadinessChecks.EvalOptionally(ctx, current)
		if ok {
			ready = &readiness.ReadyTime
		}
	} else {
		ready = status.Ready
	}

	// Evaluate the readiness of resources in the previous readiness group
	if (status == nil || !status.Reconciled) && !resource.Deleted() {
		dependencies := c.resourceClient.RangeByReadinessGroup(ctx, synRef, resource.ReadinessGroup, reconstitution.RangeDesc)
		for _, dep := range dependencies {
			slice := &apiv1.ResourceSlice{}
			err = c.client.Get(ctx, dep.ManifestRef.Slice, slice)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("getting resource slice: %w", err)
			}
			status := dep.FindStatus(slice)
			if status == nil || status.Ready == nil {
				logger.V(1).Info("skipping because at least one resource in an earlier readiness group isn't ready yet")
				return ctrl.Result{}, nil
			}
		}
	}

	// Nil current struct means the resource version hasn't changed since it was last observed
	// Skip without logging since this is a very hot path
	var modified bool
	if hasChanged {
		resource.ObserveVersion("") // in case reconciliation fails, invalidate the cache first to avoid skipping the next attempt
		modified, err = c.reconcileResource(ctx, comp, prev, resource, current)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// We requeue to make sure the resource is in sync before updating our cache's resource version
	// Otherwise the next sync would just hit the cache without actually diffing the resource.
	if modified {
		return ctrl.Result{Requeue: true}, nil
	}
	if current != nil {
		if rv := current.GetResourceVersion(); rv != "" {
			resource.ObserveVersion(rv)
		}
	}

	// Store the results
	deleted := current == nil || current.GetDeletionTimestamp() != nil
	c.writeBuffer.PatchStatusAsync(ctx, &resource.ManifestRef, patchResourceState(deleted, ready))
	if ready == nil {
		return ctrl.Result{RequeueAfter: wait.Jitter(c.readinessPollInterval, 0.1)}, nil
	}
	if resource != nil && !resource.Deleted() && resource.ReconcileInterval != nil {
		return ctrl.Result{RequeueAfter: wait.Jitter(resource.ReconcileInterval.Duration, 0.1)}, nil
	}
	return ctrl.Result{}, nil
}

func (c *Controller) reconcileResource(ctx context.Context, comp *apiv1.Composition, prev, resource *reconstitution.Resource, current *unstructured.Unstructured) (bool, error) {
	logger := logr.FromContextOrDiscard(ctx)
	start := time.Now()
	defer func() {
		reconciliationLatency.Observe(float64(time.Since(start).Milliseconds()))
	}()

	if resource.Deleted() {
		if current == nil || current.GetDeletionTimestamp() != nil {
			return false, nil // already deleted - nothing to do
		}
		if comp.Annotations["eno.azure.io/deletion-strategy"] == "orphan" {
			return false, nil
		}

		reconciliationActions.WithLabelValues("delete").Inc()
		err := c.upstreamClient.Delete(ctx, current)
		if err != nil {
			return false, client.IgnoreNotFound(fmt.Errorf("deleting resource: %w", err))
		}
		logger.V(0).Info("deleted resource")
		return true, nil
	}

	if resource.Patch != nil && current == nil {
		logger.V(1).Info("resource doesn't exist - skipping patch")
		return false, nil
	}

	// Create the resource when it doesn't exist
	if current == nil {
		reconciliationActions.WithLabelValues("create").Inc()
		obj, err := resource.Parse()
		if err != nil {
			return false, fmt.Errorf("invalid resource: %w", err)
		}
		err = c.upstreamClient.Create(ctx, obj)
		if err != nil {
			return false, fmt.Errorf("creating resource: %w", err)
		}
		logger.V(0).Info("created resource")
		return true, nil
	}

	if resource.DisableUpdates {
		return false, nil
	}

	// Compute a merge patch
	prevRV := current.GetResourceVersion()
	patch, patchType, err := c.buildPatch(ctx, prev, resource, current)
	if err != nil {
		return false, fmt.Errorf("building patch: %w", err)
	}
	if patchType != types.JSONPatchType {
		patch, err = mungePatch(patch, current.GetResourceVersion())
		if err != nil {
			return false, fmt.Errorf("adding resource version: %w", err)
		}
	}
	if len(patch) == 0 {
		logger.V(1).Info("skipping empty patch")
		return false, nil
	}
	reconciliationActions.WithLabelValues("patch").Inc()
	if insecureLogPatch {
		logger.V(1).Info("INSECURE logging patch", "patch", string(patch))
	}
	err = c.upstreamClient.Patch(ctx, current, client.RawPatch(patchType, patch))
	if err != nil {
		return false, fmt.Errorf("applying patch: %w", err)
	}
	logger.V(0).Info("patched resource", "patchType", string(patchType), "resourceVersion", current.GetResourceVersion(), "previousResourceVersion", prevRV)

	return true, nil
}

func (c *Controller) buildPatch(ctx context.Context, prev, next *reconstitution.Resource, current *unstructured.Unstructured) ([]byte, types.PatchType, error) {
	if next.Patch != nil {
		if !next.NeedsToBePatched(current) {
			return []byte{}, types.JSONPatchType, nil
		}
		patch, err := json.Marshal(&next.Patch)
		return patch, types.JSONPatchType, err
	}

	prevJS, err := prev.Finalize()
	if err != nil {
		return nil, "", reconcile.TerminalError(fmt.Errorf("building json representation of previous state: %w", err))
	}

	nextJS, err := next.Finalize()
	if err != nil {
		return nil, "", reconcile.TerminalError(fmt.Errorf("building json representation of next state: %w", err))
	}

	currentJS, err := current.MarshalJSON()
	if err != nil {
		return nil, "", reconcile.TerminalError(fmt.Errorf("building json representation of current state: %w", err))
	}

	model, err := c.discovery.Get(ctx, next.GVK)
	if err != nil {
		return nil, "", fmt.Errorf("getting merge metadata: %w", err)
	}

	// FIXME: This is a very nasty hack which should not be needed once we have
	// support for semantic equality checks.
	pdbGVK := schema.GroupVersionKind{Group: "policy", Version: "v1", Kind: "PodDisruptionBudget"}
	if model == nil || (next != nil && next.GVK == pdbGVK) {
		patch, err := jsonmergepatch.CreateThreeWayJSONMergePatch(prevJS, nextJS, currentJS)
		if err != nil {
			return nil, "", reconcile.TerminalError(err)
		}
		return patch, types.MergePatchType, err
	}

	patchmeta := strategicpatch.NewPatchMetaFromOpenAPI(model)
	patch, err := strategicpatch.CreateThreeWayMergePatch(prevJS, nextJS, currentJS, patchmeta, true)
	if err != nil {
		return nil, "", reconcile.TerminalError(err)
	}
	return patch, types.StrategicMergePatchType, err
}

func (c *Controller) getCurrent(ctx context.Context, resource *reconstitution.Resource) (*unstructured.Unstructured, bool, error) {
	if resource.HasBeenSeen() && !resource.Deleted() {
		meta := &metav1.PartialObjectMetadata{}
		meta.Name = resource.Ref.Name
		meta.Namespace = resource.Ref.Namespace
		meta.Kind = resource.GVK.Kind
		meta.APIVersion = resource.GVK.GroupVersion().String()
		err := c.upstreamClient.Get(ctx, client.ObjectKeyFromObject(meta), meta)
		if err != nil {
			return nil, false, err
		}
		if resource.MatchesLastSeen(meta.ResourceVersion) {
			return nil, false, nil
		}
		resourceVersionChanges.Inc()
	}

	current := &unstructured.Unstructured{}
	current.SetName(resource.Ref.Name)
	current.SetNamespace(resource.Ref.Namespace)
	current.SetKind(resource.GVK.Kind)
	current.SetAPIVersion(resource.GVK.GroupVersion().String())
	err := c.upstreamClient.Get(ctx, client.ObjectKeyFromObject(current), current)
	if err != nil {
		return nil, true, err
	}
	return current, true, nil
}

func mungePatch(patch []byte, rv string) ([]byte, error) {
	var patchMap map[string]interface{}
	err := json.Unmarshal(patch, &patchMap)
	if err != nil {
		return nil, reconcile.TerminalError(err)
	}
	delete(patchMap, "status")

	u := unstructured.Unstructured{Object: patchMap}
	a, err := meta.Accessor(&u)
	if err != nil {
		return nil, reconcile.TerminalError(err)
	}
	a.SetResourceVersion(rv)
	a.SetCreationTimestamp(metav1.Time{})

	if len(patchMap) <= 1 {
		return nil, nil // resource version only == empty patch
	}

	return json.Marshal(patchMap)
}

func patchResourceState(deleted bool, ready *metav1.Time) flowcontrol.StatusPatchFn {
	return func(rs *apiv1.ResourceState) *apiv1.ResourceState {
		if rs != nil && rs.Deleted == deleted && rs.Reconciled && ptr.Deref(rs.Ready, metav1.Time{}) == ptr.Deref(ready, metav1.Time{}) {
			return nil
		}
		return &apiv1.ResourceState{
			Deleted:    deleted,
			Ready:      ready,
			Reconciled: true,
		}
	}
}

// isErrMissingNS returns true when given the client-go error returned by mutating requests that do not include a namespace.
// Sadly, this error isn't exposed anywhere - it's just a plain string, so we have to do string matching here.
//
// https://github.com/kubernetes/kubernetes/blob/9edabd617945cd23111fd46cfc9a09fe37ed194a/staging/src/k8s.io/client-go/rest/request.go#L1048
func isErrMissingNS(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "an empty namespace may not be set")
}
