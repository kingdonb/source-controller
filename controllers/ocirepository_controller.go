/*
Copyright 2022 The Flux authors

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

package controllers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	soci "github.com/fluxcd/source-controller/internal/oci"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	gcrv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	kuberecorder "k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/ratelimiter"

	eventv1 "github.com/fluxcd/pkg/apis/event/v1beta1"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/oci"
	"github.com/fluxcd/pkg/oci/auth/login"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"
	rreconcile "github.com/fluxcd/pkg/runtime/reconcile"
	"github.com/fluxcd/pkg/sourceignore"
	"github.com/fluxcd/pkg/untar"
	"github.com/fluxcd/pkg/version"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	serror "github.com/fluxcd/source-controller/internal/error"
	sreconcile "github.com/fluxcd/source-controller/internal/reconcile"
	"github.com/fluxcd/source-controller/internal/reconcile/summarize"
	"github.com/fluxcd/source-controller/internal/util"
)

// ociRepositoryReadyCondition contains the information required to summarize a
// v1beta2.OCIRepository Ready Condition.
var ociRepositoryReadyCondition = summarize.Conditions{
	Target: meta.ReadyCondition,
	Owned: []string{
		sourcev1.StorageOperationFailedCondition,
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		sourcev1.ArtifactInStorageCondition,
		sourcev1.SourceVerifiedCondition,
		meta.ReadyCondition,
		meta.ReconcilingCondition,
		meta.StalledCondition,
	},
	Summarize: []string{
		sourcev1.StorageOperationFailedCondition,
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		sourcev1.ArtifactInStorageCondition,
		sourcev1.SourceVerifiedCondition,
		meta.StalledCondition,
		meta.ReconcilingCondition,
	},
	NegativePolarity: []string{
		sourcev1.StorageOperationFailedCondition,
		sourcev1.FetchFailedCondition,
		sourcev1.ArtifactOutdatedCondition,
		meta.StalledCondition,
		meta.ReconcilingCondition,
	},
}

// ociRepositoryFailConditions contains the conditions that represent a failure.
var ociRepositoryFailConditions = []string{
	sourcev1.FetchFailedCondition,
	sourcev1.StorageOperationFailedCondition,
}

type invalidOCIURLError struct {
	err error
}

func (e invalidOCIURLError) Error() string {
	return e.err.Error()
}

// ociRepositoryReconcileFunc is the function type for all the v1beta2.OCIRepository
// (sub)reconcile functions. The type implementations are grouped and
// executed serially to perform the complete reconcile of the object.
type ociRepositoryReconcileFunc func(ctx context.Context, sp *patch.SerialPatcher, obj *sourcev1.OCIRepository, metadata *sourcev1.Artifact, dir string) (sreconcile.Result, error)

// OCIRepositoryReconciler reconciles a v1beta2.OCIRepository object
type OCIRepositoryReconciler struct {
	client.Client
	helper.Metrics
	kuberecorder.EventRecorder

	Storage           *Storage
	ControllerName    string
	requeueDependency time.Duration

	patchOptions []patch.Option
}

type OCIRepositoryReconcilerOptions struct {
	MaxConcurrentReconciles   int
	DependencyRequeueInterval time.Duration
	RateLimiter               ratelimiter.RateLimiter
}

// SetupWithManager sets up the controller with the Manager.
func (r *OCIRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, OCIRepositoryReconcilerOptions{})
}

func (r *OCIRepositoryReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts OCIRepositoryReconcilerOptions) error {
	r.patchOptions = getPatchOptions(ociRepositoryReadyCondition.Owned, r.ControllerName)

	r.requeueDependency = opts.DependencyRequeueInterval

	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.OCIRepository{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: opts.MaxConcurrentReconciles,
			RateLimiter:             opts.RateLimiter,
			RecoverPanic:            true,
		}).
		Complete(r)
}

// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *OCIRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	// Fetch the OCIRepository
	obj := &sourcev1.OCIRepository{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Record suspended status metric
	r.RecordSuspend(ctx, obj, obj.Spec.Suspend)

	// Initialize the patch helper with the current version of the object.
	serialPatcher := patch.NewSerialPatcher(obj, r.Client)

	// recResult stores the abstracted reconcile result.
	var recResult sreconcile.Result

	// Always attempt to patch the object and status after each reconciliation
	// NOTE: The final runtime result and error are set in this block.
	defer func() {
		summarizeHelper := summarize.NewHelper(r.EventRecorder, serialPatcher)
		summarizeOpts := []summarize.Option{
			summarize.WithConditions(ociRepositoryReadyCondition),
			summarize.WithBiPolarityConditionTypes(sourcev1.SourceVerifiedCondition),
			summarize.WithReconcileResult(recResult),
			summarize.WithReconcileError(retErr),
			summarize.WithIgnoreNotFound(),
			summarize.WithProcessors(
				summarize.ErrorActionHandler,
				summarize.RecordReconcileReq,
			),
			summarize.WithResultBuilder(sreconcile.AlwaysRequeueResultBuilder{RequeueAfter: obj.GetRequeueAfter()}),
			summarize.WithPatchFieldOwner(r.ControllerName),
		}
		result, retErr = summarizeHelper.SummarizeAndPatch(ctx, obj, summarizeOpts...)

		// Always record readiness and duration metrics
		r.Metrics.RecordReadiness(ctx, obj)
		r.Metrics.RecordDuration(ctx, obj, start)
	}()

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(obj, sourcev1.SourceFinalizer) {
		controllerutil.AddFinalizer(obj, sourcev1.SourceFinalizer)
		recResult = sreconcile.ResultRequeue
		return
	}

	// Examine if the object is under deletion
	if !obj.ObjectMeta.DeletionTimestamp.IsZero() {
		recResult, retErr = r.reconcileDelete(ctx, obj)
		return
	}

	// Return if the object is suspended.
	if obj.Spec.Suspend {
		log.Info("reconciliation is suspended for this object")
		recResult, retErr = sreconcile.ResultEmpty, nil
		return
	}

	// Reconcile actual object
	reconcilers := []ociRepositoryReconcileFunc{
		r.reconcileStorage,
		r.reconcileSource,
		r.reconcileArtifact,
	}
	recResult, retErr = r.reconcile(ctx, serialPatcher, obj, reconcilers)
	return
}

// reconcile iterates through the ociRepositoryReconcileFunc tasks for the
// object. It returns early on the first call that returns
// reconcile.ResultRequeue, or produces an error.
func (r *OCIRepositoryReconciler) reconcile(ctx context.Context, sp *patch.SerialPatcher, obj *sourcev1.OCIRepository, reconcilers []ociRepositoryReconcileFunc) (sreconcile.Result, error) {
	oldObj := obj.DeepCopy()

	rreconcile.ProgressiveStatus(false, obj, meta.ProgressingReason, "reconciliation in progress")

	var reconcileAtVal string
	if v, ok := meta.ReconcileAnnotationValue(obj.GetAnnotations()); ok {
		reconcileAtVal = v
	}

	// Persist reconciling status if generation differs or reconciliation is
	// requested.
	switch {
	case obj.Generation != obj.Status.ObservedGeneration:
		rreconcile.ProgressiveStatus(false, obj, meta.ProgressingReason,
			"processing object: new generation %d -> %d", obj.Status.ObservedGeneration, obj.Generation)
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			return sreconcile.ResultEmpty, err
		}
	case reconcileAtVal != obj.Status.GetLastHandledReconcileRequest():
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			return sreconcile.ResultEmpty, err
		}
	}

	// Create temp working dir
	tmpDir, err := util.TempDirForObj("", obj)
	if err != nil {
		e := serror.NewGeneric(
			fmt.Errorf("failed to create temporary working directory: %w", err),
			sourcev1.DirCreationFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}
	defer func() {
		if err = os.RemoveAll(tmpDir); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "failed to remove temporary working directory")
		}
	}()
	conditions.Delete(obj, sourcev1.StorageOperationFailedCondition)

	var (
		res      sreconcile.Result
		resErr   error
		metadata = sourcev1.Artifact{}
	)

	// Run the sub-reconcilers and build the result of reconciliation.
	for _, rec := range reconcilers {
		recResult, err := rec(ctx, sp, obj, &metadata, tmpDir)
		// Exit immediately on ResultRequeue.
		if recResult == sreconcile.ResultRequeue {
			return sreconcile.ResultRequeue, nil
		}
		// If an error is received, prioritize the returned results because an
		// error also means immediate requeue.
		if err != nil {
			resErr = err
			res = recResult
			break
		}
		// Prioritize requeue request in the result.
		res = sreconcile.LowestRequeuingResult(res, recResult)
	}

	r.notify(ctx, oldObj, obj, res, resErr)

	return res, resErr
}

// reconcileSource fetches the upstream OCI artifact metadata and content.
// If this fails, it records v1beta2.FetchFailedCondition=True on the object and returns early.
func (r *OCIRepositoryReconciler) reconcileSource(ctx context.Context, sp *patch.SerialPatcher,
	obj *sourcev1.OCIRepository, metadata *sourcev1.Artifact, dir string) (sreconcile.Result, error) {
	var auth authn.Authenticator

	ctxTimeout, cancel := context.WithTimeout(ctx, obj.Spec.Timeout.Duration)
	defer cancel()

	// Remove previously failed source verification status conditions. The
	// failing verification should be recalculated. But an existing successful
	// verification need not be removed as it indicates verification of previous
	// version.
	if conditions.IsFalse(obj, sourcev1.SourceVerifiedCondition) {
		conditions.Delete(obj, sourcev1.SourceVerifiedCondition)
	}

	// Generate the registry credential keychain either from static credentials or using cloud OIDC
	keychain, err := r.keychain(ctx, obj)
	if err != nil {
		e := serror.NewGeneric(
			fmt.Errorf("failed to get credential: %w", err),
			sourcev1.AuthenticationFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	if _, ok := keychain.(soci.Anonymous); obj.Spec.Provider != sourcev1.GenericOCIProvider && ok {
		var authErr error
		auth, authErr = oidcAuth(ctxTimeout, obj.Spec.URL, obj.Spec.Provider)
		if authErr != nil && !errors.Is(authErr, oci.ErrUnconfiguredProvider) {
			e := serror.NewGeneric(
				fmt.Errorf("failed to get credential from %s: %w", obj.Spec.Provider, authErr),
				sourcev1.AuthenticationFailedReason,
			)
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}
	}

	// Generate the transport for remote operations
	transport, err := r.transport(ctx, obj)
	if err != nil {
		e := serror.NewGeneric(
			fmt.Errorf("failed to generate transport for '%s': %w", obj.Spec.URL, err),
			sourcev1.AuthenticationFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	opts := makeRemoteOptions(ctx, obj, transport, keychain, auth)

	// Determine which artifact revision to pull
	url, err := r.getArtifactURL(obj, opts.craneOpts)
	if err != nil {
		if _, ok := err.(invalidOCIURLError); ok {
			e := serror.NewStalling(
				fmt.Errorf("URL validation failed for '%s': %w", obj.Spec.URL, err),
				sourcev1.URLInvalidReason)
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}

		e := serror.NewGeneric(
			fmt.Errorf("failed to determine the artifact tag for '%s': %w", obj.Spec.URL, err),
			sourcev1.ReadOperationFailedReason)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Get the upstream revision from the artifact digest
	revision, err := r.getRevision(url, opts.craneOpts)
	if err != nil {
		e := serror.NewGeneric(
			fmt.Errorf("failed to determine artifact digest: %w", err),
			sourcev1.OCIPullFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}
	metaArtifact := &sourcev1.Artifact{Revision: revision}
	metaArtifact.DeepCopyInto(metadata)

	// Mark observations about the revision on the object
	defer func() {
		if !obj.GetArtifact().HasRevision(revision) {
			message := fmt.Sprintf("new revision '%s' for '%s'", revision, url)
			if obj.GetArtifact() != nil {
				conditions.MarkTrue(obj, sourcev1.ArtifactOutdatedCondition, "NewRevision", message)
			}
			rreconcile.ProgressiveStatus(true, obj, meta.ProgressingReason, "building artifact: %s", message)
			if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "failed to patch")
				return
			}
		}
	}()

	// Verify artifact if:
	// - the upstream digest differs from the one in storage (revision drift)
	// - the OCIRepository spec has changed (generation drift)
	// - the previous reconciliation resulted in a failed artifact verification (retry with exponential backoff)
	if obj.Spec.Verify == nil {
		// Remove old observations if verification was disabled
		conditions.Delete(obj, sourcev1.SourceVerifiedCondition)
	} else if !obj.GetArtifact().HasRevision(revision) ||
		conditions.GetObservedGeneration(obj, sourcev1.SourceVerifiedCondition) != obj.Generation ||
		conditions.IsFalse(obj, sourcev1.SourceVerifiedCondition) {

		// Insecure is not supported for verification
		if obj.Spec.Insecure {
			e := serror.NewGeneric(
				fmt.Errorf("cosign does not support insecure registries"),
				sourcev1.VerificationError,
			)
			conditions.MarkFalse(obj, sourcev1.SourceVerifiedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}

		err := r.verifySignature(ctx, obj, url, opts.verifyOpts...)
		if err != nil {
			provider := obj.Spec.Verify.Provider
			if obj.Spec.Verify.SecretRef == nil {
				provider = fmt.Sprintf("%s keyless", provider)
			}
			e := serror.NewGeneric(
				fmt.Errorf("failed to verify the signature using provider '%s': %w", provider, err),
				sourcev1.VerificationError,
			)
			conditions.MarkFalse(obj, sourcev1.SourceVerifiedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}

		conditions.MarkTrue(obj, sourcev1.SourceVerifiedCondition, meta.SucceededReason, "verified signature of revision %s", revision)
	}

	// Skip pulling if the artifact revision and the source configuration has
	// not changed.
	if obj.GetArtifact().HasRevision(revision) && !ociContentConfigChanged(obj) {
		conditions.Delete(obj, sourcev1.FetchFailedCondition)
		return sreconcile.ResultSuccess, nil
	}

	// Pull artifact from the remote container registry
	img, err := crane.Pull(url, opts.craneOpts...)
	if err != nil {
		e := serror.NewGeneric(
			fmt.Errorf("failed to pull artifact from '%s': %w", obj.Spec.URL, err),
			sourcev1.OCIPullFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Copy the OCI annotations to the internal artifact metadata
	manifest, err := img.Manifest()
	if err != nil {
		e := serror.NewGeneric(
			fmt.Errorf("failed to parse artifact manifest: %w", err),
			sourcev1.OCILayerOperationFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}
	metadata.Metadata = manifest.Annotations

	// Extract the compressed content from the selected layer
	blob, err := r.selectLayer(obj, img)
	if err != nil {
		e := serror.NewGeneric(err, sourcev1.OCILayerOperationFailedReason)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Persist layer content to storage using the specified operation
	switch obj.GetLayerOperation() {
	case sourcev1.OCILayerExtract:
		if _, err = untar.Untar(blob, dir); err != nil {
			e := serror.NewGeneric(
				fmt.Errorf("failed to extract layer contents from artifact: %w", err),
				sourcev1.OCILayerOperationFailedReason,
			)
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}
	case sourcev1.OCILayerCopy:
		metadata.Path = fmt.Sprintf("%s.tgz", r.digestFromRevision(metadata.Revision))
		file, err := os.Create(filepath.Join(dir, metadata.Path))
		if err != nil {
			e := serror.NewGeneric(
				fmt.Errorf("failed to create file to copy layer to: %w", err),
				sourcev1.OCILayerOperationFailedReason,
			)
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}
		defer file.Close()

		_, err = io.Copy(file, blob)
		if err != nil {
			e := serror.NewGeneric(
				fmt.Errorf("failed to copy layer from artifact: %w", err),
				sourcev1.OCILayerOperationFailedReason,
			)
			conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}
	default:
		e := serror.NewGeneric(
			fmt.Errorf("unsupported layer operation: %s", obj.GetLayerOperation()),
			sourcev1.OCILayerOperationFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	conditions.Delete(obj, sourcev1.FetchFailedCondition)
	return sreconcile.ResultSuccess, nil
}

// selectLayer finds the matching layer and returns its compressed contents.
// If no layer selector was provided, we pick the first layer from the OCI artifact.
func (r *OCIRepositoryReconciler) selectLayer(obj *sourcev1.OCIRepository, image gcrv1.Image) (io.ReadCloser, error) {
	layers, err := image.Layers()
	if err != nil {
		return nil, fmt.Errorf("failed to parse artifact layers: %w", err)
	}

	if len(layers) < 1 {
		return nil, fmt.Errorf("no layers found in artifact")
	}

	var layer gcrv1.Layer
	switch {
	case obj.GetLayerMediaType() != "":
		var found bool
		for i, l := range layers {
			md, err := l.MediaType()
			if err != nil {
				return nil, fmt.Errorf("failed to determine the media type of layer[%v] from artifact: %w", i, err)
			}
			if string(md) == obj.GetLayerMediaType() {
				layer = layers[i]
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("failed to find layer with media type '%s' in artifact", obj.GetLayerMediaType())
		}
	default:
		layer = layers[0]
	}

	blob, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("failed to extract the first layer from artifact: %w", err)
	}

	return blob, nil
}

// getRevision fetches the upstream digest and returns the revision in the format `<tag>/<digest>`
func (r *OCIRepositoryReconciler) getRevision(url string, options []crane.Option) (string, error) {
	ref, err := name.ParseReference(url)
	if err != nil {
		return "", err
	}

	repoTag := ""
	repoName := strings.TrimPrefix(url, ref.Context().RegistryStr())
	if s := strings.Split(repoName, ":"); len(s) == 2 && !strings.Contains(repoName, "@") {
		repoTag = s[1]
	}

	if repoTag == "" && !strings.Contains(repoName, "@") {
		repoTag = "latest"
	}

	digest, err := crane.Digest(url, options...)
	if err != nil {
		return "", err
	}

	digestHash, err := gcrv1.NewHash(digest)
	if err != nil {
		return "", err
	}

	revision := digestHash.Hex
	if repoTag != "" {
		revision = fmt.Sprintf("%s/%s", repoTag, digestHash.Hex)
	}
	return revision, nil
}

// digestFromRevision extract the digest from the revision string
func (r *OCIRepositoryReconciler) digestFromRevision(revision string) string {
	parts := strings.Split(revision, "/")
	return parts[len(parts)-1]
}

// verifySignature verifies the authenticity of the given image reference url. First, it tries using a key
// if a secret with a valid public key is provided. If not, it falls back to a keyless approach for verification.
func (r *OCIRepositoryReconciler) verifySignature(ctx context.Context, obj *sourcev1.OCIRepository, url string, opt ...remote.Option) error {
	ctxTimeout, cancel := context.WithTimeout(ctx, obj.Spec.Timeout.Duration)
	defer cancel()

	provider := obj.Spec.Verify.Provider
	switch provider {
	case "cosign":
		defaultCosignOciOpts := []soci.Options{
			soci.WithRemoteOptions(opt...),
		}

		ref, err := name.ParseReference(url)
		if err != nil {
			return err
		}

		// get the public keys from the given secret
		if secretRef := obj.Spec.Verify.SecretRef; secretRef != nil {
			certSecretName := types.NamespacedName{
				Namespace: obj.Namespace,
				Name:      secretRef.Name,
			}

			var pubSecret corev1.Secret
			if err := r.Get(ctxTimeout, certSecretName, &pubSecret); err != nil {
				return err
			}

			signatureVerified := false
			for k, data := range pubSecret.Data {
				// search for public keys in the secret
				if strings.HasSuffix(k, ".pub") {
					verifier, err := soci.NewCosignVerifier(ctxTimeout, append(defaultCosignOciOpts, soci.WithPublicKey(data))...)
					if err != nil {
						return err
					}

					signatures, _, err := verifier.VerifyImageSignatures(ctxTimeout, ref)
					if err != nil {
						continue
					}

					if signatures != nil {
						signatureVerified = true
						break
					}
				}
			}

			if !signatureVerified {
				return fmt.Errorf("no matching signatures were found for '%s'", url)
			}

			return nil
		}

		// if no secret is provided, try keyless verification
		ctrl.LoggerFrom(ctx).Info("no secret reference is provided, trying to verify the image using keyless method")
		verifier, err := soci.NewCosignVerifier(ctxTimeout, defaultCosignOciOpts...)
		if err != nil {
			return err
		}

		signatures, _, err := verifier.VerifyImageSignatures(ctxTimeout, ref)
		if err != nil {
			return err
		}

		if len(signatures) > 0 {
			return nil
		}

		return fmt.Errorf("no matching signatures were found for '%s'", url)
	}

	return nil
}

// parseRepositoryURL validates and extracts the repository URL.
func (r *OCIRepositoryReconciler) parseRepositoryURL(obj *sourcev1.OCIRepository) (string, error) {
	if !strings.HasPrefix(obj.Spec.URL, sourcev1.OCIRepositoryPrefix) {
		return "", fmt.Errorf("URL must be in format 'oci://<domain>/<org>/<repo>'")
	}

	url := strings.TrimPrefix(obj.Spec.URL, sourcev1.OCIRepositoryPrefix)
	ref, err := name.ParseReference(url)
	if err != nil {
		return "", err
	}

	imageName := strings.TrimPrefix(url, ref.Context().RegistryStr())
	if s := strings.Split(imageName, ":"); len(s) > 1 {
		return "", fmt.Errorf("URL must not contain a tag; remove ':%s'", s[1])
	}

	return ref.Context().Name(), nil
}

// getArtifactURL determines which tag or digest should be used and returns the OCI artifact FQN.
func (r *OCIRepositoryReconciler) getArtifactURL(obj *sourcev1.OCIRepository, options []crane.Option) (string, error) {
	url, err := r.parseRepositoryURL(obj)
	if err != nil {
		return "", invalidOCIURLError{err}
	}

	if obj.Spec.Reference != nil {
		if obj.Spec.Reference.Digest != "" {
			return fmt.Sprintf("%s@%s", url, obj.Spec.Reference.Digest), nil
		}

		if obj.Spec.Reference.SemVer != "" {
			tag, err := r.getTagBySemver(url, obj.Spec.Reference.SemVer, options)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s:%s", url, tag), nil
		}

		if obj.Spec.Reference.Tag != "" {
			return fmt.Sprintf("%s:%s", url, obj.Spec.Reference.Tag), nil
		}
	}

	return url, nil
}

// getTagBySemver call the remote container registry, fetches all the tags from the repository,
// and returns the latest tag according to the semver expression.
func (r *OCIRepositoryReconciler) getTagBySemver(url, exp string, options []crane.Option) (string, error) {
	tags, err := crane.ListTags(url, options...)
	if err != nil {
		return "", err
	}

	constraint, err := semver.NewConstraint(exp)
	if err != nil {
		return "", fmt.Errorf("semver '%s' parse error: %w", exp, err)
	}

	var matchingVersions []*semver.Version
	for _, t := range tags {
		v, err := version.ParseVersion(t)
		if err != nil {
			continue
		}

		if constraint.Check(v) {
			matchingVersions = append(matchingVersions, v)
		}
	}

	if len(matchingVersions) == 0 {
		return "", fmt.Errorf("no match found for semver: %s", exp)
	}

	sort.Sort(sort.Reverse(semver.Collection(matchingVersions)))
	return matchingVersions[0].Original(), nil
}

// keychain generates the credential keychain based on the resource
// configuration. If no auth is specified a default keychain with
// anonymous access is returned
func (r *OCIRepositoryReconciler) keychain(ctx context.Context, obj *sourcev1.OCIRepository) (authn.Keychain, error) {
	pullSecretNames := sets.NewString()

	// lookup auth secret
	if obj.Spec.SecretRef != nil {
		pullSecretNames.Insert(obj.Spec.SecretRef.Name)
	}

	// lookup service account
	if obj.Spec.ServiceAccountName != "" {
		serviceAccountName := obj.Spec.ServiceAccountName
		serviceAccount := corev1.ServiceAccount{}
		err := r.Get(ctx, types.NamespacedName{Namespace: obj.Namespace, Name: serviceAccountName}, &serviceAccount)
		if err != nil {
			return nil, err
		}
		for _, ips := range serviceAccount.ImagePullSecrets {
			pullSecretNames.Insert(ips.Name)
		}
	}

	// if no pullsecrets available return an AnonymousKeychain
	if len(pullSecretNames) == 0 {
		return soci.Anonymous{}, nil
	}

	// lookup image pull secrets
	imagePullSecrets := make([]corev1.Secret, len(pullSecretNames))
	for i, imagePullSecretName := range pullSecretNames.List() {
		imagePullSecret := corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Namespace: obj.Namespace, Name: imagePullSecretName}, &imagePullSecret)
		if err != nil {
			r.eventLogf(ctx, obj, eventv1.EventTypeTrace, sourcev1.AuthenticationFailedReason,
				"auth secret '%s' not found", imagePullSecretName)
			return nil, err
		}
		imagePullSecrets[i] = imagePullSecret
	}

	return k8schain.NewFromPullSecrets(ctx, imagePullSecrets)
}

// transport clones the default transport from remote and when a certSecretRef is specified,
// the returned transport will include the TLS client and/or CA certificates.
func (r *OCIRepositoryReconciler) transport(ctx context.Context, obj *sourcev1.OCIRepository) (http.RoundTripper, error) {
	if obj.Spec.CertSecretRef == nil || obj.Spec.CertSecretRef.Name == "" {
		return nil, nil
	}

	certSecretName := types.NamespacedName{
		Namespace: obj.Namespace,
		Name:      obj.Spec.CertSecretRef.Name,
	}
	var certSecret corev1.Secret
	if err := r.Get(ctx, certSecretName, &certSecret); err != nil {
		return nil, err
	}

	transport := remote.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := transport.TLSClientConfig

	if clientCert, ok := certSecret.Data[oci.ClientCert]; ok {
		// parse and set client cert and secret
		if clientKey, ok := certSecret.Data[oci.ClientKey]; ok {
			cert, err := tls.X509KeyPair(clientCert, clientKey)
			if err != nil {
				return nil, err
			}
			tlsConfig.Certificates = append(tlsConfig.Certificates, cert)
		} else {
			return nil, fmt.Errorf("'%s' found in secret, but no %s", oci.ClientCert, oci.ClientKey)
		}
	}

	if caCert, ok := certSecret.Data[oci.CACert]; ok {
		syscerts, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		syscerts.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = syscerts
	}
	return transport, nil
}

// oidcAuth generates the OIDC credential authenticator based on the specified cloud provider.
func oidcAuth(ctx context.Context, url, provider string) (authn.Authenticator, error) {
	u := strings.TrimPrefix(url, sourcev1.OCIRepositoryPrefix)
	ref, err := name.ParseReference(u)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL '%s': %w", u, err)
	}

	opts := login.ProviderOptions{}
	switch provider {
	case sourcev1.AmazonOCIProvider:
		opts.AwsAutoLogin = true
	case sourcev1.AzureOCIProvider:
		opts.AzureAutoLogin = true
	case sourcev1.GoogleOCIProvider:
		opts.GcpAutoLogin = true
	}

	return login.NewManager().Login(ctx, u, ref, opts)
}

// reconcileStorage ensures the current state of the storage matches the
// desired and previously observed state.
//
// The garbage collection is executed based on the flag configured settings and
// may remove files that are beyond their TTL or the maximum number of files
// to survive a collection cycle.
// If the Artifact in the Status of the object disappeared from the Storage,
// it is removed from the object.
// If the object does not have an Artifact in its Status, a Reconciling
// condition is added.
// The hostname of any URL in the Status of the object are updated, to ensure
// they match the Storage server hostname of current runtime.
func (r *OCIRepositoryReconciler) reconcileStorage(ctx context.Context, sp *patch.SerialPatcher,
	obj *sourcev1.OCIRepository, _ *sourcev1.Artifact, _ string) (sreconcile.Result, error) {
	// Garbage collect previous advertised artifact(s) from storage
	_ = r.garbageCollect(ctx, obj)

	// Determine if the advertised artifact is still in storage
	var artifactMissing bool
	if artifact := obj.GetArtifact(); artifact != nil && !r.Storage.ArtifactExist(*artifact) {
		obj.Status.Artifact = nil
		obj.Status.URL = ""
		artifactMissing = true
		// Remove the condition as the artifact doesn't exist.
		conditions.Delete(obj, sourcev1.ArtifactInStorageCondition)
	}

	// Record that we do not have an artifact
	if obj.GetArtifact() == nil {
		msg := "building artifact"
		if artifactMissing {
			msg += ": disappeared from storage"
		}
		rreconcile.ProgressiveStatus(true, obj, meta.ProgressingReason, msg)
		conditions.Delete(obj, sourcev1.ArtifactInStorageCondition)
		if err := sp.Patch(ctx, obj, r.patchOptions...); err != nil {
			return sreconcile.ResultEmpty, err
		}
		return sreconcile.ResultSuccess, nil
	}

	// Always update URLs to ensure hostname is up-to-date
	r.Storage.SetArtifactURL(obj.GetArtifact())
	obj.Status.URL = r.Storage.SetHostname(obj.Status.URL)

	return sreconcile.ResultSuccess, nil
}

// reconcileArtifact archives a new Artifact to the Storage, if the current
// (Status) data on the object does not match the given.
//
// The inspection of the given data to the object is differed, ensuring any
// stale observations like v1beta2.ArtifactOutdatedCondition are removed.
// If the given Artifact does not differ from the object's current, it returns
// early.
// On a successful archive, the Artifact in the Status of the object is set,
// and the symlink in the Storage is updated to its path.
func (r *OCIRepositoryReconciler) reconcileArtifact(ctx context.Context, sp *patch.SerialPatcher,
	obj *sourcev1.OCIRepository, metadata *sourcev1.Artifact, dir string) (sreconcile.Result, error) {
	revision := metadata.Revision

	// Create artifact
	artifact := r.Storage.NewArtifactFor(obj.Kind, obj, revision,
		fmt.Sprintf("%s.tar.gz", r.digestFromRevision(revision)))

	// Set the ArtifactInStorageCondition if there's no drift.
	defer func() {
		if obj.GetArtifact().HasRevision(artifact.Revision) && !ociContentConfigChanged(obj) {
			conditions.Delete(obj, sourcev1.ArtifactOutdatedCondition)
			conditions.MarkTrue(obj, sourcev1.ArtifactInStorageCondition, meta.SucceededReason,
				"stored artifact for digest '%s'", artifact.Revision)
		}
	}()

	// The artifact is up-to-date
	if obj.GetArtifact().HasRevision(artifact.Revision) && !ociContentConfigChanged(obj) {
		r.eventLogf(ctx, obj, eventv1.EventTypeTrace, sourcev1.ArtifactUpToDateReason,
			"artifact up-to-date with remote revision: '%s'", artifact.Revision)
		return sreconcile.ResultSuccess, nil
	}

	// Ensure target path exists and is a directory
	if f, err := os.Stat(dir); err != nil {
		e := serror.NewGeneric(
			fmt.Errorf("failed to stat source path: %w", err),
			sourcev1.StatOperationFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	} else if !f.IsDir() {
		e := serror.NewGeneric(
			fmt.Errorf("source path '%s' is not a directory", dir),
			sourcev1.InvalidPathReason,
		)
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}

	// Ensure artifact directory exists and acquire lock
	if err := r.Storage.MkdirAll(artifact); err != nil {
		e := serror.NewGeneric(
			fmt.Errorf("failed to create artifact directory: %w", err),
			sourcev1.DirCreationFailedReason,
		)
		conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
		return sreconcile.ResultEmpty, e
	}
	unlock, err := r.Storage.Lock(artifact)
	if err != nil {
		return sreconcile.ResultEmpty, serror.NewGeneric(
			fmt.Errorf("failed to acquire lock for artifact: %w", err),
			meta.FailedReason,
		)
	}
	defer unlock()

	switch obj.GetLayerOperation() {
	case sourcev1.OCILayerCopy:
		if err = r.Storage.CopyFromPath(&artifact, filepath.Join(dir, metadata.Path)); err != nil {
			e := serror.NewGeneric(
				fmt.Errorf("unable to copy artifact to storage: %w", err),
				sourcev1.ArchiveOperationFailedReason,
			)
			conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}
	default:
		// Load ignore rules for archiving.
		ignoreDomain := strings.Split(dir, string(filepath.Separator))
		ps, err := sourceignore.LoadIgnorePatterns(dir, ignoreDomain)
		if err != nil {
			return sreconcile.ResultEmpty, serror.NewGeneric(
				fmt.Errorf("failed to load source ignore patterns from repository: %w", err),
				"SourceIgnoreError",
			)
		}
		if obj.Spec.Ignore != nil {
			ps = append(ps, sourceignore.ReadPatterns(strings.NewReader(*obj.Spec.Ignore), ignoreDomain)...)
		}

		if err := r.Storage.Archive(&artifact, dir, SourceIgnoreFilter(ps, ignoreDomain)); err != nil {
			e := serror.NewGeneric(
				fmt.Errorf("unable to archive artifact to storage: %s", err),
				sourcev1.ArchiveOperationFailedReason,
			)
			conditions.MarkTrue(obj, sourcev1.StorageOperationFailedCondition, e.Reason, e.Err.Error())
			return sreconcile.ResultEmpty, e
		}
	}

	// Record the observations on the object.
	obj.Status.Artifact = artifact.DeepCopy()
	obj.Status.Artifact.Metadata = metadata.Metadata
	obj.Status.ContentConfigChecksum = "" // To be removed in the next API version.
	obj.Status.ObservedIgnore = obj.Spec.Ignore
	obj.Status.ObservedLayerSelector = obj.Spec.LayerSelector

	// Update symlink on a "best effort" basis
	url, err := r.Storage.Symlink(artifact, "latest.tar.gz")
	if err != nil {
		r.eventLogf(ctx, obj, eventv1.EventTypeTrace, sourcev1.SymlinkUpdateFailedReason,
			"failed to update status URL symlink: %s", err)
	}
	if url != "" {
		obj.Status.URL = url
	}
	conditions.Delete(obj, sourcev1.StorageOperationFailedCondition)
	return sreconcile.ResultSuccess, nil
}

// reconcileDelete handles the deletion of the object.
// It first garbage collects all Artifacts for the object from the Storage.
// Removing the finalizer from the object if successful.
func (r *OCIRepositoryReconciler) reconcileDelete(ctx context.Context, obj *sourcev1.OCIRepository) (sreconcile.Result, error) {
	// Garbage collect the resource's artifacts
	if err := r.garbageCollect(ctx, obj); err != nil {
		// Return the error so we retry the failed garbage collection
		return sreconcile.ResultEmpty, err
	}

	// Remove our finalizer from the list
	controllerutil.RemoveFinalizer(obj, sourcev1.SourceFinalizer)

	// Stop reconciliation as the object is being deleted
	return sreconcile.ResultEmpty, nil
}

// garbageCollect performs a garbage collection for the given object.
//
// It removes all but the current Artifact from the Storage, unless the
// deletion timestamp on the object is set. Which will result in the
// removal of all Artifacts for the objects.
func (r *OCIRepositoryReconciler) garbageCollect(ctx context.Context, obj *sourcev1.OCIRepository) error {
	if !obj.DeletionTimestamp.IsZero() {
		if deleted, err := r.Storage.RemoveAll(r.Storage.NewArtifactFor(obj.Kind, obj.GetObjectMeta(), "", "*")); err != nil {
			return serror.NewGeneric(
				fmt.Errorf("garbage collection for deleted resource failed: %w", err),
				"GarbageCollectionFailed",
			)
		} else if deleted != "" {
			r.eventLogf(ctx, obj, eventv1.EventTypeTrace, "GarbageCollectionSucceeded",
				"garbage collected artifacts for deleted resource")
		}
		obj.Status.Artifact = nil
		return nil
	}
	if obj.GetArtifact() != nil {
		delFiles, err := r.Storage.GarbageCollect(ctx, *obj.GetArtifact(), time.Second*5)
		if err != nil {
			return serror.NewGeneric(
				fmt.Errorf("garbage collection of artifacts failed: %w", err),
				"GarbageCollectionFailed",
			)
		}
		if len(delFiles) > 0 {
			r.eventLogf(ctx, obj, eventv1.EventTypeTrace, "GarbageCollectionSucceeded",
				fmt.Sprintf("garbage collected %d artifacts", len(delFiles)))
			return nil
		}
	}
	return nil
}

// eventLogf records events, and logs at the same time.
//
// This log is different from the debug log in the EventRecorder, in the sense
// that this is a simple log. While the debug log contains complete details
// about the event.
func (r *OCIRepositoryReconciler) eventLogf(ctx context.Context, obj runtime.Object, eventType string, reason string, messageFmt string, args ...interface{}) {
	msg := fmt.Sprintf(messageFmt, args...)
	// Log and emit event.
	if eventType == corev1.EventTypeWarning {
		ctrl.LoggerFrom(ctx).Error(errors.New(reason), msg)
	} else {
		ctrl.LoggerFrom(ctx).Info(msg)
	}
	r.Eventf(obj, eventType, reason, msg)
}

// notify emits notification related to the reconciliation.
func (r *OCIRepositoryReconciler) notify(ctx context.Context, oldObj, newObj *sourcev1.OCIRepository, res sreconcile.Result, resErr error) {
	// Notify successful reconciliation for new artifact and recovery from any
	// failure.
	if resErr == nil && res == sreconcile.ResultSuccess && newObj.Status.Artifact != nil {
		annotations := map[string]string{
			fmt.Sprintf("%s/%s", sourcev1.GroupVersion.Group, eventv1.MetaRevisionKey): newObj.Status.Artifact.Revision,
			fmt.Sprintf("%s/%s", sourcev1.GroupVersion.Group, eventv1.MetaChecksumKey): newObj.Status.Artifact.Checksum,
		}

		var oldChecksum string
		if oldObj.GetArtifact() != nil {
			oldChecksum = oldObj.GetArtifact().Checksum
		}

		message := fmt.Sprintf("stored artifact with revision '%s' from '%s'", newObj.Status.Artifact.Revision, newObj.Spec.URL)

		// enrich message with upstream annotations if found
		if info := newObj.GetArtifact().Metadata; info != nil {
			var source, revision string
			if val, ok := info[oci.SourceAnnotation]; ok {
				source = val
			}
			if val, ok := info[oci.RevisionAnnotation]; ok {
				revision = val
			}
			if source != "" && revision != "" {
				message = fmt.Sprintf("%s, origin source '%s', origin revision '%s'", message, source, revision)
			}
		}

		// Notify on new artifact and failure recovery.
		if oldChecksum != newObj.GetArtifact().Checksum {
			r.AnnotatedEventf(newObj, annotations, corev1.EventTypeNormal,
				"NewArtifact", message)
			ctrl.LoggerFrom(ctx).Info(message)
		} else {
			if sreconcile.FailureRecovery(oldObj, newObj, ociRepositoryFailConditions) {
				r.AnnotatedEventf(newObj, annotations, corev1.EventTypeNormal,
					meta.SucceededReason, message)
				ctrl.LoggerFrom(ctx).Info(message)
			}
		}
	}
}

// craneOptions sets the auth headers, timeout and user agent
// for all operations against remote container registries.
func craneOptions(ctx context.Context, insecure bool) []crane.Option {
	options := []crane.Option{
		crane.WithContext(ctx),
		crane.WithUserAgent(oci.UserAgent),
	}

	if insecure {
		options = append(options, crane.Insecure)
	}

	return options
}

// makeRemoteOptions returns a remoteOptions struct with the authentication and transport options set.
// The returned struct can be used to interact with a remote registry using go-containerregistry based libraries.
func makeRemoteOptions(ctxTimeout context.Context, obj *sourcev1.OCIRepository, transport http.RoundTripper,
	keychain authn.Keychain, auth authn.Authenticator) remoteOptions {
	o := remoteOptions{
		craneOpts:  craneOptions(ctxTimeout, obj.Spec.Insecure),
		verifyOpts: []remote.Option{},
	}

	if transport != nil {
		o.craneOpts = append(o.craneOpts, crane.WithTransport(transport))
		o.verifyOpts = append(o.verifyOpts, remote.WithTransport(transport))
	}

	if auth != nil {
		// auth take precedence over keychain here as we expect the caller to set
		// the auth only if it is required.
		o.verifyOpts = append(o.verifyOpts, remote.WithAuth(auth))
		o.craneOpts = append(o.craneOpts, crane.WithAuth(auth))
		return o
	}

	o.verifyOpts = append(o.verifyOpts, remote.WithAuthFromKeychain(keychain))
	o.craneOpts = append(o.craneOpts, crane.WithAuthFromKeychain(keychain))

	return o
}

// remoteOptions contains the options to interact with a remote registry.
// It can be used to pass options to go-containerregistry based libraries.
type remoteOptions struct {
	craneOpts  []crane.Option
	verifyOpts []remote.Option
}

// ociContentConfigChanged evaluates the current spec with the observations
// of the artifact in the status to determine if artifact content configuration
// has changed and requires rebuilding the artifact.
func ociContentConfigChanged(obj *sourcev1.OCIRepository) bool {
	if !pointer.StringEqual(obj.Spec.Ignore, obj.Status.ObservedIgnore) {
		return true
	}

	if !layerSelectorEqual(obj.Spec.LayerSelector, obj.Status.ObservedLayerSelector) {
		return true
	}

	return false
}

// Returns true if both arguments are nil or both arguments
// dereference to the same value.
// Based on k8s.io/utils/pointer/pointer.go pointer value equality.
func layerSelectorEqual(a, b *sourcev1.OCILayerSelector) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return *a == *b
}
