package identity

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The Secret the controller keeps its own credential in. It lives in the
// controller's namespace, not the agents' one: nothing an agent pod can read
// may contain the key that authenticates the whole cluster.
const (
	// DefaultSecretName is overridable because the chart may install two
	// controllers against two control planes in one cluster.
	DefaultSecretName = "haliphron-controller-identity"

	keyKID       = "kid"
	keySeed      = "private-key-seed"
	keyClusterID = "cluster-id"
)

// SecretStore keeps the identity in a Kubernetes Secret.
type SecretStore struct {
	Client    client.Client
	Namespace string
	Name      string
}

// NewSecretStore builds a store with the default name when none is given.
func NewSecretStore(c client.Client, namespace, name string) *SecretStore {
	if name == "" {
		name = DefaultSecretName
	}
	return &SecretStore{Client: c, Namespace: namespace, Name: name}
}

// Load reads the Secret. A missing Secret is not an error: it is the normal
// state of a cluster that has just installed the chart.
func (s *SecretStore) Load(ctx context.Context) (Persisted, bool, error) {
	var secret corev1.Secret
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &secret)
	if apierrors.IsNotFound(err) {
		return Persisted{}, false, nil
	}
	if err != nil {
		return Persisted{}, false, fmt.Errorf("read secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	seed := secret.Data[keySeed]
	if len(seed) == 0 {
		// A Secret with no key is a Secret somebody emptied by hand. Treating
		// it as absent would generate a new key and spend a bootstrap token
		// silently; saying so is better.
		return Persisted{}, false, fmt.Errorf("secret %s/%s has no %s", s.Namespace, s.Name, keySeed)
	}
	return Persisted{
		KID:       string(secret.Data[keyKID]),
		Seed:      seed,
		ClusterID: runv1.ULID(secret.Data[keyClusterID]),
	}, true, nil
}

// Save writes the Secret, creating it if it is not there. The update is a full
// replacement of the three keys and leaves anything else in the Secret alone,
// so a chart that also stores a CA bundle there is not surprised.
func (s *SecretStore) Save(ctx context.Context, p Persisted) error {
	var secret corev1.Secret
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       fields(p),
		}
		if err := s.Client.Create(ctx, &secret); err != nil {
			return fmt.Errorf("create secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read secret %s/%s: %w", s.Namespace, s.Name, err)
	}

	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	for k, v := range fields(p) {
		secret.Data[k] = v
	}
	if err := s.Client.Update(ctx, &secret); err != nil {
		return fmt.Errorf("update secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}

func fields(p Persisted) map[string][]byte {
	data := map[string][]byte{
		keyKID:  []byte(p.KID),
		keySeed: p.Seed,
	}
	if p.ClusterID != "" {
		data[keyClusterID] = []byte(p.ClusterID)
	}
	return data
}
