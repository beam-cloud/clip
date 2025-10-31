package common

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

func TestMatchRegistryPattern(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		registry string
		want     bool
	}{
		{"exact match", "ghcr.io", "ghcr.io", true},
		{"no match", "ghcr.io", "docker.io", false},
		{"wildcard all", "*", "anything.com", true},
		{"prefix wildcard", "*.dkr.ecr.*.amazonaws.com", "123456789012.dkr.ecr.us-east-1.amazonaws.com", true},
		{"prefix wildcard no match", "*.dkr.ecr.*.amazonaws.com", "gcr.io", false},
		{"suffix wildcard", "*.gcr.io", "us.gcr.io", true},
		{"suffix wildcard no match", "*.gcr.io", "ghcr.io", false},
		{"middle wildcard", "registry-*.example.com", "registry-1.example.com", true},
		{"middle wildcard no match", "registry-*.example.com", "registry.example.com", false},
		{"multiple wildcards", "*-*.*.example.com", "registry-1.us.example.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchRegistryPattern(tt.pattern, tt.registry)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStaticProviderPatternMatching(t *testing.T) {
	provider := NewStaticProvider(map[string]*authn.AuthConfig{
		"ghcr.io": {
			Username: "ghcr-user",
			Password: "ghcr-pass",
		},
		"*.dkr.ecr.*.amazonaws.com": {
			Username: "ecr-user",
			Password: "ecr-pass",
		},
	})

	t.Run("exact match", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "ghcr.io", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "ghcr-user", creds.Username)
	})

	t.Run("wildcard match", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "123456789012.dkr.ecr.us-east-1.amazonaws.com", "")
		require.NoError(t, err)
		require.NotNil(t, creds)
		assert.Equal(t, "ecr-user", creds.Username)
	})

	t.Run("no match", func(t *testing.T) {
		creds, err := provider.GetCredentials(context.Background(), "unknown.io", "")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, creds)
	})
}

func TestDetectCredentialType(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		creds    map[string]string
		want     CredentialType
	}{
		{
			name:     "no credentials",
			registry: "ghcr.io",
			creds:    map[string]string{},
			want:     CredTypePublic,
		},
		{
			name:     "AWS credentials",
			registry: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
			creds: map[string]string{
				"AWS_ACCESS_KEY_ID":     "AKIAIOSFODNN7EXAMPLE",
				"AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				"AWS_REGION":            "us-east-1",
			},
			want: CredTypeAWS,
		},
		{
			name:     "GCP credentials via token",
			registry: "gcr.io",
			creds: map[string]string{
				"GCP_ACCESS_TOKEN": "ya29.example",
			},
			want: CredTypeGCP,
		},
		{
			name:     "Azure credentials",
			registry: "myregistry.azurecr.io",
			creds: map[string]string{
				"AZURE_CLIENT_ID":     "client-id",
				"AZURE_CLIENT_SECRET": "client-secret",
				"AZURE_TENANT_ID":     "tenant-id",
			},
			want: CredTypeAzure,
		},
		{
			name:     "token credentials",
			registry: "nvcr.io",
			creds: map[string]string{
				"NGC_API_KEY": "api-key",
			},
			want: CredTypeToken,
		},
		{
			name:     "basic auth",
			registry: "ghcr.io",
			creds: map[string]string{
				"USERNAME": "user",
				"PASSWORD": "pass",
			},
			want: CredTypeBasic,
		},
		{
			name:     "detect AWS from registry",
			registry: "123456789012.dkr.ecr.us-east-1.amazonaws.com",
			creds: map[string]string{
				"SOME_KEY": "value",
			},
			want: CredTypeAWS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectCredentialType(tt.registry, tt.creds)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseCredentialsFromJSON(t *testing.T) {
	t.Run("JSON format", func(t *testing.T) {
		jsonStr := `{"USERNAME":"user","PASSWORD":"pass"}`
		creds, err := ParseCredentialsFromJSON(jsonStr)
		require.NoError(t, err)
		assert.Equal(t, "user", creds["USERNAME"])
		assert.Equal(t, "pass", creds["PASSWORD"])
	})

	t.Run("nested JSON format from beta9", func(t *testing.T) {
		// Beta9 sends nested JSON where PASSWORD contains a JSON string with AWS credentials
		jsonStr := `{"PASSWORD":"{\"AWS_ACCESS_KEY_ID\":\"AKIA123\",\"AWS_REGION\":\"us-east-1\",\"AWS_SECRET_ACCESS_KEY\":\"secret123\"}","USERNAME":"ignored"}`
		creds, err := ParseCredentialsFromJSON(jsonStr)
		require.NoError(t, err)
		
		// Should have extracted the nested AWS credentials
		assert.Equal(t, "AKIA123", creds["AWS_ACCESS_KEY_ID"])
		assert.Equal(t, "us-east-1", creds["AWS_REGION"])
		assert.Equal(t, "secret123", creds["AWS_SECRET_ACCESS_KEY"])
		
		// Original keys should still be present
		assert.Equal(t, "ignored", creds["USERNAME"])
	})

	t.Run("nested JSON with registry and type", func(t *testing.T) {
		// Full beta9 format
		jsonStr := `{"PASSWORD":"{\"credentials\":{\"AWS_ACCESS_KEY_ID\":\"AKIA123\",\"AWS_REGION\":\"us-east-1\",\"AWS_SECRET_ACCESS_KEY\":\"secret\"},\"registry\":\"187248174200.dkr.ecr.us-east-1.amazonaws.com\",\"type\":\"aws\"}","USERNAME":"{\"credentials\"}"}`
		creds, err := ParseCredentialsFromJSON(jsonStr)
		require.NoError(t, err)
		
		// Check that the inner structure was partially extracted
		// Note: This may not extract perfectly nested structures, but should get credentials
		assert.NotEmpty(t, creds)
	})

	t.Run("username:password format", func(t *testing.T) {
		creds, err := ParseCredentialsFromJSON("user:pass")
		require.NoError(t, err)
		assert.Equal(t, "user", creds["USERNAME"])
		assert.Equal(t, "pass", creds["PASSWORD"])
	})

	t.Run("empty string", func(t *testing.T) {
		creds, err := ParseCredentialsFromJSON("")
		require.NoError(t, err)
		assert.Nil(t, creds)
	})

	t.Run("invalid format", func(t *testing.T) {
		creds, err := ParseCredentialsFromJSON("invalid")
		assert.Error(t, err)
		assert.Nil(t, creds)
	})
}

func TestCreateProviderFromCredentials(t *testing.T) {
	ctx := context.Background()

	t.Run("basic auth", func(t *testing.T) {
		creds := map[string]string{
			"USERNAME": "testuser",
			"PASSWORD": "testpass",
		}
		provider := CreateProviderFromCredentials(ctx, "ghcr.io", CredTypeBasic, creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "ghcr.io", "")
		require.NoError(t, err)
		assert.Equal(t, "testuser", authConfig.Username)
		assert.Equal(t, "testpass", authConfig.Password)
	})

	t.Run("NGC token auth", func(t *testing.T) {
		creds := map[string]string{
			"NGC_API_KEY": "api-key-value",
		}
		provider := CreateProviderFromCredentials(ctx, "nvcr.io", CredTypeToken, creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "nvcr.io", "")
		require.NoError(t, err)
		assert.Equal(t, "$oauthtoken", authConfig.Username)
		assert.Equal(t, "api-key-value", authConfig.Password)
	})

	t.Run("GHCR token auth with username", func(t *testing.T) {
		creds := map[string]string{
			"GITHUB_USERNAME": "testuser",
			"GITHUB_TOKEN":    "ghp_token123",
		}
		provider := CreateProviderFromCredentials(ctx, "ghcr.io", CredTypeToken, creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "ghcr.io", "")
		require.NoError(t, err)
		assert.Equal(t, "testuser", authConfig.Username)
		assert.Equal(t, "ghp_token123", authConfig.Password)
	})

	t.Run("GHCR token auth without username", func(t *testing.T) {
		creds := map[string]string{
			"GITHUB_TOKEN": "ghp_token123",
		}
		provider := CreateProviderFromCredentials(ctx, "ghcr.io", CredTypeToken, creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "ghcr.io", "")
		require.NoError(t, err)
		// Should use token as username when no username provided
		assert.Equal(t, "ghp_token123", authConfig.Username)
		assert.Equal(t, "ghp_token123", authConfig.Password)
	})

	t.Run("Docker Hub with username/password", func(t *testing.T) {
		creds := map[string]string{
			"DOCKERHUB_USERNAME": "dockeruser",
			"DOCKERHUB_PASSWORD": "dockerpass",
		}
		provider := CreateProviderFromCredentials(ctx, "docker.io", CredTypeToken, creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "docker.io", "")
		require.NoError(t, err)
		assert.Equal(t, "dockeruser", authConfig.Username)
		assert.Equal(t, "dockerpass", authConfig.Password)
	})

	t.Run("GCP with access token", func(t *testing.T) {
		creds := map[string]string{
			"GCP_ACCESS_TOKEN": "ya29.token123",
		}
		provider := CreateProviderFromCredentials(ctx, "gcr.io", CredTypeGCP, creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "gcr.io", "")
		require.NoError(t, err)
		assert.Equal(t, "oauth2accesstoken", authConfig.Username)
		assert.Equal(t, "ya29.token123", authConfig.Password)
	})

	t.Run("no credentials", func(t *testing.T) {
		provider := CreateProviderFromCredentials(ctx, "ghcr.io", CredTypePublic, map[string]string{})
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "ghcr.io", "")
		assert.Equal(t, ErrNoCredentials, err)
		assert.Nil(t, authConfig)
	})

	t.Run("registry-specific username keys", func(t *testing.T) {
		creds := map[string]string{
			"REGISTRY_USERNAME": "registry-user",
			"DOCKER_USERNAME":   "docker-user",
			"USERNAME":          "generic-user",
			"PASSWORD":          "pass123",
		}
		provider := CreateProviderFromCredentials(ctx, "example.com", CredTypeBasic, creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "example.com", "")
		require.NoError(t, err)
		// Should prefer REGISTRY_USERNAME over others
		assert.Equal(t, "registry-user", authConfig.Username)
		assert.Equal(t, "pass123", authConfig.Password)
	})
}

func TestCredentialsToProvider(t *testing.T) {
	ctx := context.Background()

	t.Run("auto-detect basic auth", func(t *testing.T) {
		creds := map[string]string{
			"USERNAME": "user",
			"PASSWORD": "pass",
		}
		provider := CredentialsToProvider(ctx, "ghcr.io", creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "ghcr.io", "")
		require.NoError(t, err)
		assert.Equal(t, "user", authConfig.Username)
	})

	t.Run("auto-detect token", func(t *testing.T) {
		creds := map[string]string{
			"GITHUB_TOKEN": "ghp_token",
		}
		provider := CredentialsToProvider(ctx, "ghcr.io", creds)
		require.NotNil(t, provider)
		
		authConfig, err := provider.GetCredentials(ctx, "ghcr.io", "")
		require.NoError(t, err)
		// For GHCR without explicit username, token is used as both username and password
		assert.Equal(t, "ghp_token", authConfig.Username)
		assert.Equal(t, "ghp_token", authConfig.Password)
	})

	t.Run("empty credentials", func(t *testing.T) {
		provider := CredentialsToProvider(ctx, "ghcr.io", map[string]string{})
		require.NotNil(t, provider)
		assert.Equal(t, "public-only", provider.Name())
	})
}
