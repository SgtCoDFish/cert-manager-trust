/*
Copyright 2021 The cert-manager Authors.

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

package bundle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	trustapi "github.com/cert-manager/trust-manager/pkg/apis/trust/v1alpha1"
	"github.com/cert-manager/trust-manager/pkg/util"
)

type notFoundError struct{ error }

// bundleData holds the result of a call to buildSourceBundle. It contains both the resulting PEM-encoded
// certificate data from concatenating all of the sources together and any metadata from the sources which
// needs to be exposed on the Bundle resource's status field.
type bundleData struct {
	data string

	sourceVersions []string
}

// buildSourceBundle retrieves and concatenates all source bundle data for this Bundle object.
// Each source data is validated and pruned to ensure that all certificates within are valid, and
// is each bundle is concatenated together with a new line character.
func (b *bundle) buildSourceBundle(ctx context.Context, bundle *trustapi.Bundle) (bundleData, error) {
	var resolvedBundle bundleData
	var bundles []string
	var versions []string

	for _, source := range bundle.Spec.Sources {
		var (
			sourceData string
			version    string
			err        error
		)

		switch {
		case source.ConfigMap != nil:
			sourceData, version, err = b.configMapBundle(ctx, source.ConfigMap)

		case source.Secret != nil:
			sourceData, version, err = b.secretBundle(ctx, source.Secret)

		case source.InLine != nil:
			sourceData = *source.InLine
			version = fmt.Sprintf("generation: %d", bundle.GetGeneration())

		case source.UseDefaultCAs != nil && *source.UseDefaultCAs:
			if b.defaultPackage == nil {
				err = notFoundError{fmt.Errorf("no default package was specified when trust-manager was started; default CAs not available")}
			} else {
				sourceData = b.defaultPackage.Bundle
				version = b.defaultPackage.StringID()
			}
		}

		if err != nil {
			return bundleData{}, fmt.Errorf("failed to retrieve bundle from source: %w", err)
		}

		sanitizedBundle, err := util.ValidateAndSanitizePEMBundle([]byte(sourceData))
		if err != nil {
			return bundleData{}, fmt.Errorf("invalid PEM data in source: %w", err)
		}

		bundles = append(bundles, string(sanitizedBundle))
		versions = append(versions, version)
	}

	// NB: bundles should never be empty here, since ValidateAndSanitizePEMBundle errors when a bundle source
	// contains no valid certificates. Plus, the webhook validation should confirm that there's at least one source
	// defined to avoid otherwise empty bundles.
	// Still, just in case, we check and return an error in case somehow an empty bundle snuck through.

	if len(bundles) == 0 {
		return bundleData{}, fmt.Errorf("couldn't find any valid certificates in bundle")
	}

	resolvedBundle.data = strings.Join(bundles, "\n") + "\n"
	resolvedBundle.sourceVersions = versions

	return resolvedBundle, nil
}

// configMapBundle returns the data in the target ConfigMap within the trust Namespace.
func (b *bundle) configMapBundle(ctx context.Context, ref *trustapi.SourceObjectKeySelector) (string, string, error) {
	var configMap corev1.ConfigMap
	err := b.client.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ref.Name}, &configMap)
	if apierrors.IsNotFound(err) {
		return "", "", notFoundError{err}
	}

	if err != nil {
		return "", "", fmt.Errorf("failed to get ConfigMap %s/%s: %w", b.Namespace, ref.Name, err)
	}

	data, ok := configMap.Data[ref.Key]
	if !ok {
		return "", "", notFoundError{fmt.Errorf("no data found in ConfigMap %s/%s at key %q", b.Namespace, ref.Name, ref.Key)}
	}

	return data, fmt.Sprintf("generation: %d", configMap.Generation), nil
}

// secretBundle returns the data in the target Secret within the trust Namespace.
func (b *bundle) secretBundle(ctx context.Context, ref *trustapi.SourceObjectKeySelector) (string, string, error) {
	var secret corev1.Secret
	err := b.client.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: ref.Name}, &secret)
	if apierrors.IsNotFound(err) {
		return "", "", notFoundError{err}
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to get Secret %s/%s: %w", b.Namespace, ref.Name, err)
	}

	data, ok := secret.Data[ref.Key]
	if !ok {
		return "", "", notFoundError{fmt.Errorf("no data found in Secret %s/%s at key %q", b.Namespace, ref.Name, ref.Key)}
	}

	return string(data), fmt.Sprintf("generation: %d", secret.Generation), nil
}

// syncTarget syncs the given data to the target ConfigMap in the given namespace.
// The name of the ConfigMap is the same as the Bundle.
// Ensures the ConfigMap is owned by the given Bundle, and the data is up to date.
// Returns true if the ConfigMap has been created or was updated.
func (b *bundle) syncTarget(ctx context.Context, log logr.Logger,
	bundle *trustapi.Bundle,
	namespaceSelector labels.Selector,
	namespace *corev1.Namespace,
	data string,
) (bool, error) {
	target := bundle.Spec.Target

	if target.ConfigMap == nil {
		return false, errors.New("target not defined")
	}

	matchNamespace := namespaceSelector.Matches(labels.Set(namespace.Labels))

	var configMap corev1.ConfigMap
	err := b.client.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: bundle.Name}, &configMap)

	// If the ConfigMap doesn't exist yet, create it.
	if apierrors.IsNotFound(err) {
		// If the namespace doesn't match selector we do nothing since we don't
		// want to create it, and it also doesn't exist.
		if !matchNamespace {
			log.V(4).Info("ignoring namespace as it doesn't match selector", "labels", namespace.Labels)
			return false, nil
		}

		configMap = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            bundle.Name,
				Namespace:       namespace.Name,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(bundle, trustapi.SchemeGroupVersion.WithKind("Bundle"))},
			},
			Data: map[string]string{
				target.ConfigMap.Key: data,
			},
		}

		return true, b.client.Create(ctx, &configMap)
	}

	if err != nil {
		return false, fmt.Errorf("failed to get configmap %s/%s: %w", namespace, bundle.Name, err)
	}

	// Here, the config map exists, but the selector doesn't match the namespace.
	if !matchNamespace {
		// The ConfigMap is owned by this controller- delete it.
		if metav1.IsControlledBy(&configMap, bundle) {
			log.V(2).Info("deleting bundle from Namespace since namespaceSelector does not match")
			return true, b.client.Delete(ctx, &configMap)
		}
		// The ConfigMap isn't owned by us, so we shouldn't delete it. Return that
		// we did nothing.
		b.recorder.Eventf(&configMap, corev1.EventTypeWarning, "NotOwned", "ConfigMap is not owned by trust.cert-manager.io so ignoring")
		return false, nil
	}

	var needsUpdate bool
	// If ConfigMap is missing OwnerReference, add it back.
	if !metav1.IsControlledBy(&configMap, bundle) {
		configMap.OwnerReferences = append(configMap.OwnerReferences, *metav1.NewControllerRef(bundle, trustapi.SchemeGroupVersion.WithKind("Bundle")))
		needsUpdate = true
	}

	// Match, return do nothing
	if cmdata, ok := configMap.Data[target.ConfigMap.Key]; !ok || cmdata != data {
		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}
		configMap.Data[target.ConfigMap.Key] = data
		needsUpdate = true
	}

	// Exit early if no update is needed
	if !needsUpdate {
		return false, nil
	}

	if err := b.client.Update(ctx, &configMap); err != nil {
		return true, fmt.Errorf("failed to update configmap %s/%s with bundle: %w", namespace, bundle.Name, err)
	}

	log.V(2).Info("synced bundle to namespace")

	return true, nil
}
