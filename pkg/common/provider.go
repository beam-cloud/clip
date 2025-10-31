package common

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/rs/zerolog/log"
)

var (
	// ErrNoCredentials indicates that no credentials are available for the requested registry
	ErrNoCredentials = errors.New("no credentials available")
)

// RegistryCredentialProvider is the core interface for obtaining registry credentials
// at runtime. This interface enables pluggable authentication strategies without
// persisting credentials in archive metadata.
//
// Implementations should:
//   - Return credentials dynamically (support token refresh)
//   - Return ErrNoCredentials if credentials are not available (caller will try anonymous)
//   - Handle short-lived tokens gracefully (e.g., ECR, GCR)
//   - Never log or expose sensitive credential data
type RegistryCredentialProvider interface {
	// GetCredentials returns authentication configuration for a given registry.
	//
	// Parameters:
	//   - ctx: context for cancellation and timeouts
	//   - registry: registry hostname (e.g., "ghcr.io", "registry-1.docker.io")
	//   - scope: optional repository path for per-repo tokens (e.g., "beam-cloud/clip")
	//
	// Returns:
	//   - *authn.AuthConfig: credentials if available
	//   - error: ErrNoCredentials if unavailable, or other error if lookup failed
	GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error)

	// Name returns a human-readable name for this provider (for logging/debugging)
	Name() string
}

// PublicOnlyProvider always returns ErrNoCredentials, forcing anonymous/public access
type PublicOnlyProvider struct{}

func NewPublicOnlyProvider() *PublicOnlyProvider {
	return &PublicOnlyProvider{}
}

func (p *PublicOnlyProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	return nil, ErrNoCredentials
}

func (p *PublicOnlyProvider) Name() string {
	return "public-only"
}

// StaticProvider returns pre-configured credentials for specific registries
type StaticProvider struct {
	credentials map[string]*authn.AuthConfig
}

// NewStaticProvider creates a provider with a fixed set of credentials
// The map key should be the registry hostname (e.g., "ghcr.io")
func NewStaticProvider(credentials map[string]*authn.AuthConfig) *StaticProvider {
	return &StaticProvider{
		credentials: credentials,
	}
}

func (p *StaticProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	if creds, ok := p.credentials[registry]; ok {
		log.Debug().
			Str("registry", registry).
			Str("provider", "static").
			Msg("found credentials in static provider")
		return creds, nil
	}
	return nil, ErrNoCredentials
}

func (p *StaticProvider) Name() string {
	return "static"
}

// DockerConfigProvider reads credentials from Docker's config.json
type DockerConfigProvider struct {
	configPath string
}

// NewDockerConfigProvider creates a provider that reads from Docker config
// If configPath is empty, uses default location (~/.docker/config.json or $DOCKER_CONFIG)
func NewDockerConfigProvider(configPath string) *DockerConfigProvider {
	if configPath == "" {
		// Check DOCKER_CONFIG env var
		if dockerConfig := os.Getenv("DOCKER_CONFIG"); dockerConfig != "" {
			configPath = filepath.Join(dockerConfig, "config.json")
		} else {
			// Default to ~/.docker/config.json
			if home, err := os.UserHomeDir(); err == nil {
				configPath = filepath.Join(home, ".docker", "config.json")
			}
		}
	}

	return &DockerConfigProvider{
		configPath: configPath,
	}
}

func (p *DockerConfigProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	if p.configPath == "" {
		return nil, ErrNoCredentials
	}

	// Read Docker config file
	data, err := os.ReadFile(p.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCredentials
		}
		return nil, fmt.Errorf("failed to read Docker config: %w", err)
	}

	var config struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}

	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse Docker config: %w", err)
	}

	// Try exact match first
	if auth, ok := config.Auths[registry]; ok && auth.Auth != "" {
		return decodeDockerAuth(auth.Auth)
	}

	// Try with https:// prefix (Docker sometimes stores with protocol)
	if auth, ok := config.Auths["https://"+registry]; ok && auth.Auth != "" {
		return decodeDockerAuth(auth.Auth)
	}

	// Try registry-1.docker.io variations for Docker Hub
	if registry == "index.docker.io" || registry == "docker.io" || registry == "registry-1.docker.io" {
		for _, variant := range []string{"https://index.docker.io/v1/", "index.docker.io", "docker.io", "registry-1.docker.io"} {
			if auth, ok := config.Auths[variant]; ok && auth.Auth != "" {
				log.Debug().
					Str("registry", registry).
					Str("matched_variant", variant).
					Str("provider", "docker-config").
					Msg("found Docker Hub credentials using variant")
				return decodeDockerAuth(auth.Auth)
			}
		}
	}

	return nil, ErrNoCredentials
}

func (p *DockerConfigProvider) Name() string {
	return "docker-config"
}

// decodeDockerAuth decodes base64-encoded "username:password" from Docker config
func decodeDockerAuth(encoded string) (*authn.AuthConfig, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode auth: %w", err)
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid auth format")
	}

	return &authn.AuthConfig{
		Username: parts[0],
		Password: parts[1],
	}, nil
}

// EnvProvider reads credentials from environment variables
// Supports multiple formats:
//   - CLIP_REGISTRY_USER_<HOST> / CLIP_REGISTRY_PASS_<HOST>
//   - CLIP_OCI_AUTH (JSON format)
type EnvProvider struct{}

func NewEnvProvider() *EnvProvider {
	return &EnvProvider{}
}

func (p *EnvProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	// Normalize registry for env var lookup (replace . and - with _)
	normalizedRegistry := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(registry, ".", "_"), "-", "_"))

	// Try CLIP_REGISTRY_USER_<HOST> format
	userKey := fmt.Sprintf("CLIP_REGISTRY_USER_%s", normalizedRegistry)
	passKey := fmt.Sprintf("CLIP_REGISTRY_PASS_%s", normalizedRegistry)

	if user := os.Getenv(userKey); user != "" {
		pass := os.Getenv(passKey)
		log.Debug().
			Str("registry", registry).
			Str("provider", "env").
			Str("user_key", userKey).
			Msg("found credentials in environment variables")
		return &authn.AuthConfig{
			Username: user,
			Password: pass,
		}, nil
	}

	// Try CLIP_OCI_AUTH JSON format
	if authJSON := os.Getenv("CLIP_OCI_AUTH"); authJSON != "" {
		var authMap map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Token    string `json:"token"`
		}

		if err := json.Unmarshal([]byte(authJSON), &authMap); err == nil {
			if auth, ok := authMap[registry]; ok {
				log.Debug().
					Str("registry", registry).
					Str("provider", "env").
					Msg("found credentials in CLIP_OCI_AUTH JSON")

				// Prefer token over username/password
				if auth.Token != "" {
					return &authn.AuthConfig{
						Username: "oauth2accesstoken",
						Password: auth.Token,
					}, nil
				}
				return &authn.AuthConfig{
					Username: auth.Username,
					Password: auth.Password,
				}, nil
			}
		}
	}

	return nil, ErrNoCredentials
}

func (p *EnvProvider) Name() string {
	return "env"
}

// KeychainProvider wraps go-containerregistry's keychain (supports Docker, GCR, ECR, etc.)
type KeychainProvider struct {
	keychain authn.Keychain
}

// NewKeychainProvider creates a provider using go-containerregistry's default keychain
// This automatically handles Docker config, GCR, ECR, and other standard auth methods
func NewKeychainProvider() *KeychainProvider {
	return &KeychainProvider{
		keychain: authn.DefaultKeychain,
	}
}

func (p *KeychainProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	// Parse registry as a name.Registry to create a Resource
	reg, err := name.NewRegistry(registry)
	if err != nil {
		return nil, fmt.Errorf("failed to parse registry: %w", err)
	}

	// Try to get authenticator from keychain
	auth, err := p.keychain.Resolve(reg)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve auth: %w", err)
	}

	// Get auth config
	authConfig, err := auth.Authorization()
	if err != nil {
		return nil, fmt.Errorf("failed to get authorization: %w", err)
	}

	// If we got credentials, return them
	if authConfig != nil && (authConfig.Username != "" || authConfig.RegistryToken != "" || authConfig.IdentityToken != "") {
		log.Debug().
			Str("registry", registry).
			Str("provider", "keychain").
			Msg("found credentials in keychain")

		// Convert to authn.AuthConfig
		return &authn.AuthConfig{
			Username:      authConfig.Username,
			Password:      authConfig.Password,
			Auth:          authConfig.Auth,
			IdentityToken: authConfig.IdentityToken,
			RegistryToken: authConfig.RegistryToken,
		}, nil
	}

	return nil, ErrNoCredentials
}

func (p *KeychainProvider) Name() string {
	return "keychain"
}

// ChainedProvider tries multiple providers in order until one succeeds
type ChainedProvider struct {
	providers []RegistryCredentialProvider
}

// NewChainedProvider creates a provider that tries each provider in order
func NewChainedProvider(providers ...RegistryCredentialProvider) *ChainedProvider {
	return &ChainedProvider{
		providers: providers,
	}
}

func (p *ChainedProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	for _, provider := range p.providers {
		creds, err := provider.GetCredentials(ctx, registry, scope)
		if err == nil && creds != nil {
			log.Debug().
				Str("registry", registry).
				Str("provider", provider.Name()).
				Msg("credentials found in chained provider")
			return creds, nil
		}
		if err != nil && err != ErrNoCredentials {
			log.Debug().
				Err(err).
				Str("registry", registry).
				Str("provider", provider.Name()).
				Msg("provider returned error, trying next")
		}
	}
	return nil, ErrNoCredentials
}

func (p *ChainedProvider) Name() string {
	names := make([]string, len(p.providers))
	for i, provider := range p.providers {
		names[i] = provider.Name()
	}
	return fmt.Sprintf("chain[%s]", strings.Join(names, ","))
}

// CallbackProvider allows custom credential resolution logic
type CallbackProvider struct {
	callback func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error)
	name     string
}

// NewCallbackProvider creates a provider with custom resolution logic
func NewCallbackProvider(callback func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error)) *CallbackProvider {
	return &CallbackProvider{
		callback: callback,
		name:     "callback",
	}
}

// NewCallbackProviderWithName creates a named callback provider
func NewCallbackProviderWithName(name string, callback func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error)) *CallbackProvider {
	return &CallbackProvider{
		callback: callback,
		name:     name,
	}
}

func (p *CallbackProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	return p.callback(ctx, registry, scope)
}

func (p *CallbackProvider) Name() string {
	return p.name
}

// CachingProvider wraps another provider with caching and TTL support
// This is useful for short-lived tokens (ECR, GCR) that need periodic refresh
type CachingProvider struct {
	base  RegistryCredentialProvider
	cache map[string]*cachedCredential
	ttl   time.Duration
	mu    sync.RWMutex
}

type cachedCredential struct {
	config    *authn.AuthConfig
	expiresAt time.Time
}

// NewCachingProvider creates a provider that caches credentials with a TTL
func NewCachingProvider(base RegistryCredentialProvider, ttl time.Duration) *CachingProvider {
	return &CachingProvider{
		base:  base,
		cache: make(map[string]*cachedCredential),
		ttl:   ttl,
	}
}

func (p *CachingProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	cacheKey := registry
	if scope != "" {
		cacheKey = fmt.Sprintf("%s/%s", registry, scope)
	}

	// Check cache
	p.mu.RLock()
	if cached, ok := p.cache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		p.mu.RUnlock()
		log.Debug().
			Str("registry", registry).
			Str("scope", scope).
			Str("provider", "caching").
			Msg("using cached credentials")
		return cached.config, nil
	}
	p.mu.RUnlock()

	// Cache miss or expired - fetch from base provider
	config, err := p.base.GetCredentials(ctx, registry, scope)
	if err != nil {
		return nil, err
	}

	// Cache the result
	p.mu.Lock()
	p.cache[cacheKey] = &cachedCredential{
		config:    config,
		expiresAt: time.Now().Add(p.ttl),
	}
	p.mu.Unlock()

	log.Debug().
		Str("registry", registry).
		Str("scope", scope).
		Str("provider", "caching").
		Dur("ttl", p.ttl).
		Msg("cached new credentials")

	return config, nil
}

func (p *CachingProvider) Name() string {
	return fmt.Sprintf("caching[%s]", p.base.Name())
}

// DefaultProvider returns a sensible default provider chain for most use cases
// Order: Env -> Docker Config -> Keychain
func DefaultProvider() RegistryCredentialProvider {
	return NewChainedProvider(
		NewEnvProvider(),
		NewDockerConfigProvider(""),
		NewKeychainProvider(),
	)
}

// ParseBase64AuthConfig parses the legacy base64-encoded auth config format
// Returns a StaticProvider with the decoded credentials
// This is used for backward compatibility with the old AuthConfig field
func ParseBase64AuthConfig(encoded string, registry string) (*StaticProvider, error) {
	if encoded == "" {
		return nil, ErrNoCredentials
	}

	// Decode base64
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode auth config: %w", err)
	}

	// Parse as JSON
	var config authn.AuthConfig
	if err := json.Unmarshal(decoded, &config); err != nil {
		return nil, fmt.Errorf("failed to parse auth config: %w", err)
	}

	log.Warn().
		Str("registry", registry).
		Msg("DEPRECATED: using base64 inline auth config - prefer external auth providers")

	return NewStaticProvider(map[string]*authn.AuthConfig{
		registry: &config,
	}), nil
}
