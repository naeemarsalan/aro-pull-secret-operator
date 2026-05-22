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

package globalpullsecret

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// shortHash returns the first 12 hex chars of a SHA-256 digest. It is
// used purely for human-readable identification on annotations and in
// log lines and is not security-sensitive.
func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// dockerConfigJSON is the on-disk shape of a Kubernetes pull secret.
// We use map[string]any for auths so we round-trip auth entries
// (which may carry "auth", "username", "password", "email", or
// provider-specific fields) without lossy re-marshalling.
type dockerConfigJSON struct {
	Auths map[string]any `json:"auths"`
}

// Validate returns the raw dockerconfigjson bytes if the secret holds
// a syntactically valid pull secret with at least one auth entry.
func Validate(s *corev1.Secret) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("secret is nil")
	}
	raw, ok := s.Data[corev1.DockerConfigJsonKey]
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("secret %s/%s missing %q key",
			s.Namespace, s.Name, corev1.DockerConfigJsonKey)
	}
	var cfg dockerConfigJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("secret %s/%s: invalid dockerconfigjson: %w",
			s.Namespace, s.Name, err)
	}
	if len(cfg.Auths) == 0 {
		return nil, fmt.Errorf("secret %s/%s: dockerconfigjson has no auth entries",
			s.Namespace, s.Name)
	}
	return raw, nil
}

// Merge combines an original pull secret with zero or more additional
// pull secrets. On registry conflict the original entry wins; this
// matches HyperShift's globalps semantics and prevents an additional
// secret from displacing cluster credentials for registries the cluster
// already authenticates against.
//
// Callers wanting to add credentials for a registry the cluster already
// uses should use namespaced registry paths (e.g. quay.io/myorg) so the
// keys do not collide.
func Merge(original []byte, additional ...[]byte) ([]byte, []string, error) {
	base, err := decodeAuths(original)
	if err != nil {
		return nil, nil, fmt.Errorf("decode original: %w", err)
	}
	merged := make(map[string]any, len(base))
	for k, v := range base {
		merged[k] = v
	}

	var conflicts []string
	for i, addBytes := range additional {
		add, err := decodeAuths(addBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("decode additional[%d]: %w", i, err)
		}
		for registry, auth := range add {
			if _, exists := merged[registry]; exists {
				conflicts = append(conflicts, registry)
				continue
			}
			merged[registry] = auth
		}
	}

	out, err := json.Marshal(dockerConfigJSON{Auths: merged})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal merged: %w", err)
	}
	return out, conflicts, nil
}

func decodeAuths(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var cfg dockerConfigJSON
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg.Auths == nil {
		cfg.Auths = map[string]any{}
	}
	return cfg.Auths, nil
}
