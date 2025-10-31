package registryauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublicOnlyProvider(t *testing.T) {
	provider := NewPublicOnlyProvider()
	
	creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
	assert.Equal(t, ErrNoCredentials, err)
	assert.Nil(t, creds)
	assert.Equal(t, "public-only", provider.Name())
}

func TestStaticProvider(t *testing.T) {
	provider := NewStaticProvider(map[string]*authn.AuthConfig{
		"ghcr.io": {
			Username: "testuser",
			Password: "testpass",
		},
		"registry-1.docker.io": {
			Username: "dockeruser",
			Password: "dockerpass",
		},
	})

	t.Run("found credentials", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "beam-cloud/clip")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "testuser", creds.Username)
		assert.Equal(t, "testpass", creds.Password)
	})

	t.Run("no credentials", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "unknown.io", "")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, creds)
	})

	t.Run("provider name", func(t *testing.T) {
		assert.Equal(t, "static", provider.Name())
	})
}

func TestDockerConfigProvider(t *testing.T) {
	// Create temporary Docker config
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	// Create Docker config with credentials
	dockerConfig := map[string]interface{}{
		"auths": map[string]interface{}{
			"ghcr.io": map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte("testuser:testpass")),
			},
			"https://index.docker.io/v1/": map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte("dockeruser:dockerpass")),
			},
		},
	}

	configData, err := json.Marshal(dockerConfig)
	require.NoError(t, err)
	err = os.WriteFile(configPath, configData, 0644)
	require.NoError(t, err)

	provider := NewDockerConfigProvider(configPath)

	t.Run("found credentials", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "testuser", creds.Username)
		assert.Equal(t, "testpass", creds.Password)
	})

	t.Run("docker hub variants", func(t *testing.T) {
		// Test various Docker Hub registry names
		for _, registry := range []string{"index.docker.io", "docker.io", "registry-1.docker.io"} {
			creds, err := provider.GetCredentials(context.Background(), registry, "")
			require.NoError(t, err, "failed for registry: %s", registry)
			require.NotNil(t, creds, "nil credentials for registry: %s", registry)
			assert.Equal(t, "dockeruser", creds.Username)
			assert.Equal(t, "dockerpass", creds.Password)
		}
	})

	t.Run("no credentials", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "unknown.io", "")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, creds)
	})

	t.Run("provider name", func(t *testing.T) {
		assert.Equal(t, "docker-config", provider.Name())
	})

	t.Run("nonexistent config file", func(t *testing.T) {
		provider := NewDockerConfigProvider("/nonexistent/config.json")
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, creds)
	})
}

func TestEnvProvider(t *testing.T) {
	provider := NewEnvProvider()

	t.Run("individual env vars", func(t *testing.T) {
		os.Setenv("CLIP_REGISTRY_USER_GHCR_IO", "envuser")
		os.Setenv("CLIP_REGISTRY_PASS_GHCR_IO", "envpass")
		defer os.Unsetenv("CLIP_REGISTRY_USER_GHCR_IO")
		defer os.Unsetenv("CLIP_REGISTRY_PASS_GHCR_IO")

		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "envuser", creds.Username)
		assert.Equal(t, "envpass", creds.Password)
	})

	t.Run("JSON format", func(t *testing.T) {
		authJSON := map[string]interface{}{
			"ghcr.io": map[string]string{
				"username": "jsonuser",
				"password": "jsonpass",
			},
			"registry.io": map[string]string{
				"token": "tokenvalue",
			},
		}
		authData, _ := json.Marshal(authJSON)
		os.Setenv("CLIP_OCI_AUTH", string(authData))
		defer os.Unsetenv("CLIP_OCI_AUTH")

		// Test username/password
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "jsonuser", creds.Username)
		assert.Equal(t, "jsonpass", creds.Password)

		// Test token (should use oauth2accesstoken username)
		creds, err = provider.GetCredentials(context.Background(), "registry.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "oauth2accesstoken", creds.Username)
		assert.Equal(t, "tokenvalue", creds.Password)
	})

	t.Run("no credentials", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "unknown.io", "")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, creds)
	})

	t.Run("provider name", func(t *testing.T) {
		assert.Equal(t, "env", provider.Name())
	})

	t.Run("normalized registry names", func(t *testing.T) {
		// Test that registry names with dots and dashes are normalized
		os.Setenv("CLIP_REGISTRY_USER_123456789_DKR_ECR_US_EAST_1_AMAZONAWS_COM", "ecruser")
		os.Setenv("CLIP_REGISTRY_PASS_123456789_DKR_ECR_US_EAST_1_AMAZONAWS_COM", "ecrpass")
		defer os.Unsetenv("CLIP_REGISTRY_USER_123456789_DKR_ECR_US_EAST_1_AMAZONAWS_COM")
		defer os.Unsetenv("CLIP_REGISTRY_PASS_123456789_DKR_ECR_US_EAST_1_AMAZONAWS_COM")

		creds, err := provider.GetCredentials(context.Background(), "123456789.dkr.ecr.us-east-1.amazonaws.com", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "ecruser", creds.Username)
		assert.Equal(t, "ecrpass", creds.Password)
	})
}

func TestChainedProvider(t *testing.T) {
	provider1 := NewStaticProvider(map[string]*authn.AuthConfig{
		"ghcr.io": {
			Username: "user1",
			Password: "pass1",
		},
	})

	provider2 := NewStaticProvider(map[string]*authn.AuthConfig{
		"docker.io": {
			Username: "user2",
			Password: "pass2",
		},
	})

	provider3 := NewStaticProvider(map[string]*authn.AuthConfig{
		"gcr.io": {
			Username: "user3",
			Password: "pass3",
		},
	})

	chained := NewChainedProvider(provider1, provider2, provider3)

	t.Run("first provider succeeds", func(t *testing.T) {
		creds, err := chained.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "user1", creds.Username)
	})

	t.Run("second provider succeeds", func(t *testing.T) {
		creds, err := chained.GetCredentials(context.Background(), "docker.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "user2", creds.Username)
	})

	t.Run("third provider succeeds", func(t *testing.T) {
		creds, err := chained.GetCredentials(context.Background(), "gcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "user3", creds.Username)
	})

	t.Run("no provider succeeds", func(t *testing.T) {
		creds, err := chained.GetCredentials(context.Background(), "unknown.io", "")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, creds)
	})

	t.Run("provider name", func(t *testing.T) {
		assert.Contains(t, chained.Name(), "chain")
		assert.Contains(t, chained.Name(), "static")
	})
}

func TestCallbackProvider(t *testing.T) {
	callCount := 0
	callback := func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
		callCount++
		if registry == "ghcr.io" {
			return &authn.AuthConfig{
				Username: "callback-user",
				Password: "callback-pass",
			}, nil
		}
		return nil, ErrNoCredentials
	}

	provider := NewCallbackProvider(callback)

	t.Run("callback succeeds", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "beam-cloud/clip")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "callback-user", creds.Username)
		assert.Equal(t, "callback-pass", creds.Password)
		assert.Equal(t, 1, callCount)
	})

	t.Run("callback returns no credentials", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "unknown.io", "")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, creds)
		assert.Equal(t, 2, callCount)
	})

	t.Run("provider name", func(t *testing.T) {
		assert.Equal(t, "callback", provider.Name())
	})

	t.Run("custom name", func(t *testing.T) {
		namedProvider := NewCallbackProviderWithName("my-custom-provider", callback)
		assert.Equal(t, "my-custom-provider", namedProvider.Name())
	})
}

func TestCachingProvider(t *testing.T) {
	callCount := 0
	baseProvider := NewCallbackProvider(func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
		callCount++
		if registry == "ghcr.io" {
			return &authn.AuthConfig{
				Username: "cached-user",
				Password: "cached-pass",
			}, nil
		}
		return nil, ErrNoCredentials
	})

	provider := NewCachingProvider(baseProvider, 100*time.Millisecond)

	t.Run("first call fetches from base", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "cached-user", creds.Username)
		assert.Equal(t, 1, callCount)
	})

	t.Run("second call uses cache", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "cached-user", creds.Username)
		assert.Equal(t, 1, callCount, "should not have called base provider again")
	})

	t.Run("cache expires", func(t *testing.T) {
		time.Sleep(150 * time.Millisecond) // Wait for cache to expire
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "cached-user", creds.Username)
		assert.Equal(t, 2, callCount, "should have called base provider again after expiry")
	})

	t.Run("different scope has separate cache", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "different-scope")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, 3, callCount, "should have called base provider for different scope")
	})

	t.Run("provider name", func(t *testing.T) {
		assert.Contains(t, provider.Name(), "caching")
		assert.Contains(t, provider.Name(), "callback")
	})
}

func TestDefaultProvider(t *testing.T) {
	provider := DefaultProvider()
	
	// Should be a chained provider
	assert.NotNil(t, provider)
	assert.Contains(t, provider.Name(), "chain")
	
	// Should include env, docker-config, and keychain
	assert.Contains(t, provider.Name(), "env")
	assert.Contains(t, provider.Name(), "docker-config")
	assert.Contains(t, provider.Name(), "keychain")
}

func TestParseBase64AuthConfig(t *testing.T) {
	t.Run("valid auth config", func(t *testing.T) {
		config := authn.AuthConfig{
			Username: "testuser",
			Password: "testpass",
		}
		configJSON, _ := json.Marshal(config)
		encoded := base64.StdEncoding.EncodeToString(configJSON)

		provider, err := ParseBase64AuthConfig(encoded, "ghcr.io")
		require.NoError(t, err)
		require.NotNil(t, provider)

		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "testuser", creds.Username)
		assert.Equal(t, "testpass", creds.Password)
	})

	t.Run("empty auth config", func(t *testing.T) {
		provider, err := ParseBase64AuthConfig("", "ghcr.io")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, provider)
	})

	t.Run("invalid base64", func(t *testing.T) {
		provider, err := ParseBase64AuthConfig("not-valid-base64!", "ghcr.io")
		assert.Error(t, err)
		assert.Nil(t, provider)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		encoded := base64.StdEncoding.EncodeToString([]byte("{invalid json"))
		provider, err := ParseBase64AuthConfig(encoded, "ghcr.io")
		assert.Error(t, err)
		assert.Nil(t, provider)
	})
}

func TestDecodeDockerAuth(t *testing.T) {
	t.Run("valid auth", func(t *testing.T) {
		encoded := base64.StdEncoding.EncodeToString([]byte("username:password"))
		config, err := decodeDockerAuth(encoded)
		require.NoError(t, err)
		require.NotNil(t, config)
		assert.Equal(t, "username", config.Username)
		assert.Equal(t, "password", config.Password)
	})

	t.Run("invalid base64", func(t *testing.T) {
		config, err := decodeDockerAuth("not-valid-base64!")
		assert.Error(t, err)
		assert.Nil(t, config)
	})

	t.Run("invalid format", func(t *testing.T) {
		encoded := base64.StdEncoding.EncodeToString([]byte("no-colon"))
		config, err := decodeDockerAuth(encoded)
		assert.Error(t, err)
		assert.Nil(t, config)
	})

	t.Run("password with colon", func(t *testing.T) {
		// Password can contain colons, only first colon is the delimiter
		encoded := base64.StdEncoding.EncodeToString([]byte("username:pass:word:with:colons"))
		config, err := decodeDockerAuth(encoded)
		require.NoError(t, err)
		require.NotNil(t, config)
		assert.Equal(t, "username", config.Username)
		assert.Equal(t, "pass:word:with:colons", config.Password)
	})
}
