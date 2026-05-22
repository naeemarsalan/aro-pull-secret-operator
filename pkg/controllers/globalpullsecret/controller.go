// Copyright 2026 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package globalpullsecret reconciles the cluster pull secret
// (openshift-config/pull-secret) by merging it with one or more
// user-supplied additional pull secrets discovered in kube-system.
//
// Design note: HyperShift's globalps controller deploys a DaemonSet that
// writes /var/lib/kubelet/config.json on each node. That design relies on
// HyperShift "Replace" node pools where no MCD is reconciling kubelet
// config post-boot. On classic OpenShift (ARO included) the Machine
// Config Operator owns that file, so we cannot safely write to it from
// a DaemonSet. Instead we write the merged result back to
// openshift-config/pull-secret and let MCO propagate it to nodes via
// the same render pipeline it uses for any cluster-wide pull-secret
// change.
package globalpullsecret

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// ControllerName is the controller-runtime controller name; used in
	// the leader-election lock key and in log lines.
	ControllerName = "global-pull-secret"

	// ClusterPullSecretNamespace is where OCP stores the cluster-wide
	// pull secret consumed by MCO.
	ClusterPullSecretNamespace = "openshift-config"
	// ClusterPullSecretName is the well-known name of that secret.
	ClusterPullSecretName = "pull-secret"

	// AdditionalSecretsNamespace is where the operator looks for
	// additional pull secrets to merge in.
	AdditionalSecretsNamespace = "kube-system"
	// AdditionalSecretLabel is the label key the operator selects on.
	// A Secret with this label set to AdditionalSecretLabelValue and of
	// type kubernetes.io/dockerconfigjson is merged into the cluster
	// pull secret. Multiple such secrets are merged in lexical order.
	AdditionalSecretLabel      = "pullsecret.openshift.io/include"
	AdditionalSecretLabelValue = "additional"

	// OriginalSecretName is the in-cluster snapshot of the cluster
	// pull secret as it existed before this operator first wrote to it.
	// The operator restores from this snapshot when no additional
	// secrets are present, and uses it as the merge base on every
	// reconcile so that subsequent runs are not order-dependent.
	OriginalSecretName = "original-pull-secret"

	// ManagedByAnnotation marks openshift-config/pull-secret as written
	// by this operator. The annotation is what tells the bootstrap path
	// not to re-snapshot the current cluster pull secret as the
	// "original" when this operator restarts.
	ManagedByAnnotation = "pullsecret.openshift.io/managed-by"
	ManagedByValue      = "aro-pull-secret-operator"

	// ContentHashAnnotation lets external tooling (and humans) tell at
	// a glance whether the cluster pull secret matches the last merged
	// result the operator computed.
	ContentHashAnnotation = "pullsecret.openshift.io/content-hash"
)

// staticRequest is the single reconcile.Request all watches enqueue.
// The reconciler does not key off individual objects — it always
// reconciles the entire pull-secret state machine.
var staticRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: ControllerName}}

// Reconciler merges the cluster pull secret with any additional pull
// secrets labeled in kube-system.
type Reconciler struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// SetupWithManager wires the controller into mgr. The controller has
// exactly one logical work item, so every relevant event is collapsed
// to the same reconcile.Request via staticEventHandler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.Scheme = mgr.GetScheme()
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor(ControllerName)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		Watches(
			&corev1.Secret{},
			staticEventHandler(),
			builder.WithPredicates(secretPredicate()),
		).
		Complete(r)
}

// staticEventHandler turns any matched event into the single shared
// reconcile.Request used by the controller.
func staticEventHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{staticRequest}
	})
}

// secretPredicate accepts the well-known cluster pull secret, the
// in-cluster original-pull-secret snapshot, and any kube-system Secret
// carrying the include label. Everything else is filtered out at the
// informer level to avoid wakeups on unrelated secret churn.
func secretPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		switch {
		case o.GetNamespace() == ClusterPullSecretNamespace && o.GetName() == ClusterPullSecretName:
			return true
		case o.GetNamespace() == AdditionalSecretsNamespace && o.GetName() == OriginalSecretName:
			return true
		case o.GetNamespace() == AdditionalSecretsNamespace:
			return o.GetLabels()[AdditionalSecretLabel] == AdditionalSecretLabelValue
		default:
			return false
		}
	})
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,namespace=openshift-config,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,namespace=kube-system,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile merges the snapshot of the original cluster pull secret
// with each labeled additional secret in kube-system and writes the
// result back to openshift-config/pull-secret. It is safe to call when
// nothing has changed: the write is a no-op if the merged content
// already matches the cluster pull secret.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	cluster := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ClusterPullSecretNamespace,
		Name:      ClusterPullSecretName,
	}, cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("get cluster pull secret: %w", err)
	}

	originalBytes, err := r.ensureOriginalSnapshot(ctx, cluster)
	if err != nil {
		return reconcile.Result{}, err
	}

	additionalBytes, err := r.collectAdditional(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	merged, conflicts, err := Merge(originalBytes, additionalBytes...)
	if err != nil {
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "MergeFailed", "Merge of additional pull secrets failed: %v", err)
		return reconcile.Result{}, fmt.Errorf("merge: %w", err)
	}
	for _, registry := range conflicts {
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "RegistryConflict",
			"Registry %q already present in cluster pull secret; additional entry ignored", registry)
		log.Info("registry conflict; original wins", "registry", registry)
	}

	if err := r.writeCluster(ctx, cluster, merged); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// ensureOriginalSnapshot returns the bytes that should be treated as
// the merge base. On first run (no snapshot, no managed annotation) it
// captures the current cluster pull secret as the snapshot and returns
// those bytes. On subsequent runs it returns the existing snapshot.
//
// The snapshot lets the operator stay idempotent and order-independent:
// adding and then removing an additional secret returns the cluster
// pull secret to exactly its pre-operator state.
func (r *Reconciler) ensureOriginalSnapshot(ctx context.Context, cluster *corev1.Secret) ([]byte, error) {
	snapshot := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: AdditionalSecretsNamespace,
		Name:      OriginalSecretName,
	}, snapshot)
	switch {
	case err == nil:
		raw, ok := snapshot.Data[corev1.DockerConfigJsonKey]
		if !ok || len(raw) == 0 {
			return nil, fmt.Errorf("snapshot %s/%s missing %q",
				snapshot.Namespace, snapshot.Name, corev1.DockerConfigJsonKey)
		}
		return raw, nil
	case apierrors.IsNotFound(err):
		// fall through to bootstrap
	default:
		return nil, fmt.Errorf("get original snapshot: %w", err)
	}

	if cluster.GetAnnotations()[ManagedByAnnotation] == ManagedByValue {
		// Operator wrote the cluster pull secret previously but the
		// snapshot is gone. Refuse to bootstrap from a derived value:
		// doing so would bake additional registries into the new
		// "original" and they could never be cleanly removed.
		return nil, fmt.Errorf("cluster pull secret is operator-managed but snapshot %s/%s is missing; "+
			"restore the snapshot before continuing",
			AdditionalSecretsNamespace, OriginalSecretName)
	}

	raw, err := Validate(cluster)
	if err != nil {
		return nil, fmt.Errorf("cluster pull secret is not valid dockerconfigjson: %w", err)
	}

	newSnapshot := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: AdditionalSecretsNamespace,
			Name:      OriginalSecretName,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, newSnapshot, func() error {
		if newSnapshot.Annotations == nil {
			newSnapshot.Annotations = map[string]string{}
		}
		newSnapshot.Annotations[ManagedByAnnotation] = ManagedByValue
		newSnapshot.Type = corev1.SecretTypeDockerConfigJson
		newSnapshot.Data = map[string][]byte{corev1.DockerConfigJsonKey: raw}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("create original snapshot: %w", err)
	}
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "SnapshotCreated",
		"Captured original cluster pull secret as %s/%s", newSnapshot.Namespace, newSnapshot.Name)
	return raw, nil
}

// collectAdditional returns the dockerconfigjson bytes of every Secret
// in kube-system carrying the include label. Invalid secrets are
// skipped (with an event) so a single bad entry does not block valid
// ones.
func (r *Reconciler) collectAdditional(ctx context.Context) ([][]byte, error) {
	list := &corev1.SecretList{}
	if err := r.Client.List(ctx, list,
		client.InNamespace(AdditionalSecretsNamespace),
		client.MatchingLabels{AdditionalSecretLabel: AdditionalSecretLabelValue},
	); err != nil {
		return nil, fmt.Errorf("list additional secrets: %w", err)
	}

	out := make([][]byte, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		if s.Type != corev1.SecretTypeDockerConfigJson {
			r.Recorder.Eventf(s, corev1.EventTypeWarning, "WrongType",
				"Secret type is %q; expected %q. Skipping.", s.Type, corev1.SecretTypeDockerConfigJson)
			continue
		}
		raw, err := Validate(s)
		if err != nil {
			r.Recorder.Eventf(s, corev1.EventTypeWarning, "InvalidSecret",
				"Skipping additional pull secret: %v", err)
			continue
		}
		out = append(out, raw)
	}
	return out, nil
}

// writeCluster updates openshift-config/pull-secret with the merged
// content if it differs from what's there. It tags the secret with the
// managed-by annotation so a subsequent operator restart can tell
// it apart from a user-authored value.
func (r *Reconciler) writeCluster(ctx context.Context, cluster *corev1.Secret, merged []byte) error {
	current := cluster.Data[corev1.DockerConfigJsonKey]
	if bytes.Equal(current, merged) {
		return nil
	}

	patch := client.MergeFrom(cluster.DeepCopy())
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[ManagedByAnnotation] = ManagedByValue
	cluster.Annotations[ContentHashAnnotation] = shortHash(merged)
	cluster.Data[corev1.DockerConfigJsonKey] = merged

	if err := r.Client.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("patch cluster pull secret: %w", err)
	}
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "Updated",
		"Updated %s/%s with merged pull secret (hash %s)",
		cluster.Namespace, cluster.Name, cluster.Annotations[ContentHashAnnotation])
	return nil
}
