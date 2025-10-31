package registryauth_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/beam-cloud/clip/pkg/registryauth"
	"github.com/google/go-containerregistry/pkg/authn"
)

// Example_publicRegistry demonstrates accessing a public registry
// No credentials are needed
func Example_publicRegistry() {
	provider := registryauth.NewPublicOnlyProvider()
	
	ctx := context.Background()
	creds, err := provider.GetCredentials(ctx, "docker.io", "library/nginx")
	
	if err == registryauth.ErrNoCredentials {
		fmt.Println("Public access - no credentials needed")
	} else if err != nil {
		fmt.Printf("Error: %v\n", err)
	} else {
		fmt.Printf("Got credentials for user: %s\n", creds.Username)
	}
	
	// Output:
	// Public access - no credentials needed
}

// Example_staticCredentials demonstrates using static credentials
func Example_staticCredentials() {
	provider := registryauth.NewStaticProvider(map[string]*authn.AuthConfig{
		"ghcr.io": {
			Username: "myuser",
			Password: "ghp_mytoken",
		},
	})
	
	ctx := context.Background()
	creds, err := provider.GetCredentials(ctx, "ghcr.io", "myorg/myrepo")
	
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	
	fmt.Printf("Provider: %s\n", provider.Name())
	fmt.Printf("Found credentials for: %s\n", creds.Username)
	
	// Output:
	// Provider: static
	// Found credentials for: myuser
}

// Example_environmentVariables demonstrates using environment variables
func Example_environmentVariables() {
	// Set up environment variables
	os.Setenv("CLIP_REGISTRY_USER_GHCR_IO", "envuser")
	os.Setenv("CLIP_REGISTRY_PASS_GHCR_IO", "envtoken")
	defer os.Unsetenv("CLIP_REGISTRY_USER_GHCR_IO")
	defer os.Unsetenv("CLIP_REGISTRY_PASS_GHCR_IO")
	
	provider := registryauth.NewEnvProvider()
	
	ctx := context.Background()
	creds, err := provider.GetCredentials(ctx, "ghcr.io", "")
	
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	
	fmt.Printf("Provider: %s\n", provider.Name())
	fmt.Printf("Found credentials for: %s\n", creds.Username)
	
	// Output:
	// Provider: env
	// Found credentials for: envuser
}

// Example_chainedProviders demonstrates trying multiple providers
func Example_chainedProviders() {
	// Create a chain that tries multiple sources
	provider := registryauth.NewChainedProvider(
		registryauth.NewEnvProvider(),                // Try environment first
		registryauth.NewDockerConfigProvider(""),     // Then Docker config
		registryauth.NewKeychainProvider(),           // Finally keychain
	)
	
	fmt.Printf("Provider chain: %s\n", provider.Name())
	
	// This would try each provider in order
	// For this example, we just show the setup
	
	// Output:
	// Provider chain: chain[env,docker-config,keychain]
}

// Example_customCallback demonstrates custom credential logic
func Example_customCallback() {
	// Custom callback that simulates fetching from a secret service
	provider := registryauth.NewCallbackProvider(func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
		// Your custom logic here
		if registry == "my-private-registry.io" {
			// Simulate fetching from a secrets service
			return &authn.AuthConfig{
				Username: "service-account",
				Password: "dynamically-fetched-token",
			}, nil
		}
		return nil, registryauth.ErrNoCredentials
	})
	
	ctx := context.Background()
	creds, err := provider.GetCredentials(ctx, "my-private-registry.io", "myorg/myrepo")
	
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	
	fmt.Printf("Provider: %s\n", provider.Name())
	fmt.Printf("Found credentials for: %s\n", creds.Username)
	
	// Output:
	// Provider: callback
	// Found credentials for: service-account
}

// Example_cachingProvider demonstrates caching for expensive lookups
func Example_cachingProvider() {
	callCount := 0
	
	// Base provider that simulates an expensive operation
	baseProvider := registryauth.NewCallbackProvider(func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
		callCount++
		fmt.Printf("Fetching credentials (call #%d)\n", callCount)
		
		// Simulate expensive operation (e.g., calling AWS STS)
		return &authn.AuthConfig{
			Username: "AWS",
			Password: "sts-token-123456",
		}, nil
	})
	
	// Wrap with caching (15 minute TTL)
	provider := registryauth.NewCachingProvider(baseProvider, 15*time.Minute)
	
	ctx := context.Background()
	
	// First call - fetches from base provider
	creds1, _ := provider.GetCredentials(ctx, "123456.dkr.ecr.us-east-1.amazonaws.com", "")
	fmt.Printf("First call: %s\n", creds1.Username)
	
	// Second call - uses cache
	creds2, _ := provider.GetCredentials(ctx, "123456.dkr.ecr.us-east-1.amazonaws.com", "")
	fmt.Printf("Second call: %s (from cache)\n", creds2.Username)
	
	// Output:
	// Fetching credentials (call #1)
	// First call: AWS
	// Second call: AWS (from cache)
}

// Example_defaultProvider demonstrates the default provider chain
func Example_defaultProvider() {
	// DefaultProvider() returns a sensible default chain:
	// env -> docker-config -> keychain
	provider := registryauth.DefaultProvider()
	
	fmt.Printf("Default provider: %s\n", provider.Name())
	
	// This provider will work in most environments without configuration
	
	// Output:
	// Default provider: chain[env,docker-config,keychain]
}

// Example_multipleRegistries demonstrates handling multiple registries
func Example_multipleRegistries() {
	provider := registryauth.NewStaticProvider(map[string]*authn.AuthConfig{
		"ghcr.io": {
			Username: "github-user",
			Password: "ghp_token",
		},
		"registry-1.docker.io": {
			Username: "docker-user",
			Password: "docker-token",
		},
		"123456.dkr.ecr.us-east-1.amazonaws.com": {
			Username: "AWS",
			Password: "ecr-token",
		},
	})
	
	ctx := context.Background()
	
	// Get credentials for different registries
	registries := []string{
		"ghcr.io",
		"registry-1.docker.io",
		"123456.dkr.ecr.us-east-1.amazonaws.com",
	}
	
	for _, registry := range registries {
		creds, err := provider.GetCredentials(ctx, registry, "")
		if err != nil {
			fmt.Printf("%s: no credentials\n", registry)
		} else {
			fmt.Printf("%s: %s\n", registry, creds.Username)
		}
	}
	
	// Output:
	// ghcr.io: github-user
	// registry-1.docker.io: docker-user
	// 123456.dkr.ecr.us-east-1.amazonaws.com: AWS
}

// Example_tokenRefresh demonstrates handling expiring tokens
func Example_tokenRefresh() {
	// Simulate a token that expires and needs refresh
	tokenVersion := 0
	
	provider := registryauth.NewCallbackProvider(func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
		tokenVersion++
		token := fmt.Sprintf("token-v%d", tokenVersion)
		
		fmt.Printf("Issuing new token: %s\n", token)
		
		return &authn.AuthConfig{
			Username: "oauth2accesstoken",
			Password: token,
		}, nil
	})
	
	ctx := context.Background()
	
	// First mount
	creds1, _ := provider.GetCredentials(ctx, "gcr.io", "myproject/myimage")
	fmt.Printf("First mount: %s\n", creds1.Password)
	
	// Token expires, re-auth on next operation
	creds2, _ := provider.GetCredentials(ctx, "gcr.io", "myproject/myimage")
	fmt.Printf("After expiry: %s\n", creds2.Password)
	
	// Output:
	// Issuing new token: token-v1
	// First mount: token-v1
	// Issuing new token: token-v2
	// After expiry: token-v2
}

// Example_errorHandling demonstrates proper error handling
func Example_errorHandling() {
	provider := registryauth.NewStaticProvider(map[string]*authn.AuthConfig{
		"ghcr.io": {
			Username: "myuser",
			Password: "mytoken",
		},
	})
	
	ctx := context.Background()
	
	// Try to get credentials for unknown registry
	creds, err := provider.GetCredentials(ctx, "unknown-registry.io", "")
	
	if err == registryauth.ErrNoCredentials {
		fmt.Println("No credentials available - will try anonymous access")
	} else if err != nil {
		fmt.Printf("Unexpected error: %v\n", err)
	} else {
		fmt.Printf("Got credentials: %s\n", creds.Username)
	}
	
	// Output:
	// No credentials available - will try anonymous access
}

// Example_scopeSpecificCredentials demonstrates per-repository credentials
func Example_scopeSpecificCredentials() {
	provider := registryauth.NewCallbackProvider(func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
		// You can use the scope parameter for per-repo tokens
		if scope == "private-org/secret-repo" {
			return &authn.AuthConfig{
				Username: "service-account-1",
				Password: "high-privilege-token",
			}, nil
		} else if scope != "" {
			return &authn.AuthConfig{
				Username: "service-account-2",
				Password: "read-only-token",
			}, nil
		}
		return nil, registryauth.ErrNoCredentials
	})
	
	ctx := context.Background()
	
	// Different credentials based on repository
	creds1, _ := provider.GetCredentials(ctx, "ghcr.io", "private-org/secret-repo")
	fmt.Printf("Secret repo: %s\n", creds1.Username)
	
	creds2, _ := provider.GetCredentials(ctx, "ghcr.io", "public-org/normal-repo")
	fmt.Printf("Normal repo: %s\n", creds2.Username)
	
	// Output:
	// Secret repo: service-account-1
	// Normal repo: service-account-2
}
