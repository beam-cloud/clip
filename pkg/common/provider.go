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

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
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
// Supports both exact registry matching and wildcard patterns
type StaticProvider struct {
	credentials map[string]*authn.AuthConfig
	name        string // optional custom name for debugging
}

// NewStaticProvider creates a provider with a fixed set of credentials
// The map key should be the registry hostname (e.g., "ghcr.io")
// Supports patterns like "*.dkr.ecr.*.amazonaws.com" for wildcard matching
func NewStaticProvider(credentials map[string]*authn.AuthConfig) *StaticProvider {
	return &StaticProvider{
		credentials: credentials,
		name:        "static",
	}
}

// NewStaticProviderWithName creates a named static provider
func NewStaticProviderWithName(name string, credentials map[string]*authn.AuthConfig) *StaticProvider {
	return &StaticProvider{
		credentials: credentials,
		name:        name,
	}
}

func (p *StaticProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	// Try exact match first
	if creds, ok := p.credentials[registry]; ok {
		log.Debug().
			Str("registry", registry).
			Str("provider", p.name).
			Msg("found credentials in static provider (exact match)")
		return creds, nil
	}

	// Try pattern matching (e.g., "*.dkr.ecr.*.amazonaws.com")
	for pattern, creds := range p.credentials {
		if matchRegistryPattern(pattern, registry) {
			log.Debug().
				Str("registry", registry).
				Str("pattern", pattern).
				Str("provider", p.name).
				Msg("found credentials in static provider (pattern match)")
			return creds, nil
		}
	}

	return nil, ErrNoCredentials
}

func (p *StaticProvider) Name() string {
	return p.name
}

// matchRegistryPattern checks if a registry matches a pattern with wildcards
// Supports * as wildcard (e.g., "*.dkr.ecr.*.amazonaws.com" matches "123456789012.dkr.ecr.us-east-1.amazonaws.com")
func matchRegistryPattern(pattern, registry string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == registry
	}

	// Simple wildcard matching
	patternParts := strings.Split(pattern, "*")
	if len(patternParts) == 0 {
		return false
	}

	// Check prefix
	if patternParts[0] != "" && !strings.HasPrefix(registry, patternParts[0]) {
		return false
	}

	// Check suffix
	if patternParts[len(patternParts)-1] != "" && !strings.HasSuffix(registry, patternParts[len(patternParts)-1]) {
		return false
	}

	// Check middle parts
	currentPos := 0
	for i, part := range patternParts {
		if part == "" {
			continue
		}
		idx := strings.Index(registry[currentPos:], part)
		if idx == -1 {
			return false
		}
		if i == 0 && idx != 0 {
			return false
		}
		currentPos += idx + len(part)
	}

	return true
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

// ECRProvider provides AWS ECR credentials by calling the ECR GetAuthorizationToken API
// This provider handles AWS ECR registries and fetches temporary tokens dynamically
type ECRProvider struct {
	awsAccessKey    string
	awsSecretKey    string
	awsSessionToken string
	awsRegion       string
	registryPattern string // e.g., "*.dkr.ecr.*.amazonaws.com"
	cache           *cachedCredential
	cacheTTL        time.Duration
	mu              sync.RWMutex
}

// ECRProviderConfig configures an ECR provider
type ECRProviderConfig struct {
	AWSAccessKey    string
	AWSSecretKey    string
	AWSSessionToken string        // optional
	AWSRegion       string
	RegistryPattern string        // optional, defaults to "*.dkr.ecr.*.amazonaws.com"
	CacheTTL        time.Duration // optional, defaults to 11 hours (ECR tokens valid for 12h)
}

// NewECRProvider creates a provider that fetches ECR authorization tokens
func NewECRProvider(config ECRProviderConfig) *ECRProvider {
	pattern := config.RegistryPattern
	if pattern == "" {
		pattern = "*.dkr.ecr.*.amazonaws.com"
	}

	ttl := config.CacheTTL
	if ttl == 0 {
		ttl = 11 * time.Hour // ECR tokens valid for 12h, refresh at 11h
	}

	return &ECRProvider{
		awsAccessKey:    config.AWSAccessKey,
		awsSecretKey:    config.AWSSecretKey,
		awsSessionToken: config.AWSSessionToken,
		awsRegion:       config.AWSRegion,
		registryPattern: pattern,
		cacheTTL:        ttl,
	}
}

func (p *ECRProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	// Check if this registry matches our pattern
	if !matchRegistryPattern(p.registryPattern, registry) {
		log.Debug().
			Str("registry", registry).
			Str("pattern", p.registryPattern).
			Str("scope", scope).
			Msg("ECR provider: registry does not match pattern")
		return nil, ErrNoCredentials
	}

	// Check cache
	p.mu.RLock()
	if p.cache != nil && time.Now().Before(p.cache.expiresAt) {
		p.mu.RUnlock()
		log.Debug().
			Str("registry", registry).
			Str("provider", "ecr").
			Msg("using cached ECR credentials")
		return p.cache.config, nil
	}
	p.mu.RUnlock()

	// Fetch new token from ECR
	log.Info().
		Str("registry", registry).
		Str("region", p.awsRegion).
		Str("provider", "ecr").
		Str("pattern", p.registryPattern).
		Msg("fetching new ECR authorization token")

	// Configure AWS client
	credProvider := credentials.NewStaticCredentialsProvider(
		p.awsAccessKey,
		p.awsSecretKey,
		p.awsSessionToken,
	)

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(p.awsRegion),
		awsconfig.WithCredentialsProvider(credProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Get ECR authorization token
	client := ecr.NewFromConfig(cfg)
	output, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get ECR token: %w", err)
	}

	if len(output.AuthorizationData) == 0 || output.AuthorizationData[0].AuthorizationToken == nil {
		return nil, fmt.Errorf("no authorization data returned from ECR")
	}

	// Decode base64 token (format: "AWS:base64token")
	base64Token := aws.ToString(output.AuthorizationData[0].AuthorizationToken)
	decodedToken, err := base64.StdEncoding.DecodeString(base64Token)
	if err != nil {
		return nil, fmt.Errorf("failed to decode ECR token: %w", err)
	}

	// Parse username:password
	parts := strings.SplitN(string(decodedToken), ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid ECR token format")
	}

	authConfig := &authn.AuthConfig{
		Username: parts[0],
		Password: parts[1],
	}

	// Cache the result
	p.mu.Lock()
	p.cache = &cachedCredential{
		config:    authConfig,
		expiresAt: time.Now().Add(p.cacheTTL),
	}
	p.mu.Unlock()

	log.Info().
		Str("registry", registry).
		Str("region", p.awsRegion).
		Str("provider", "ecr").
		Dur("ttl", p.cacheTTL).
		Msg("fetched and cached ECR authorization token")

	return authConfig, nil
}

func (p *ECRProvider) Name() string {
	return fmt.Sprintf("ecr[%s]", p.awsRegion)
}

// AWSCredentialProvider provides credentials for any AWS-based registry by setting env vars
// and using the keychain provider (which handles ECR, etc.)
type AWSCredentialProvider struct {
	awsAccessKey    string
	awsSecretKey    string
	awsSessionToken string
	awsRegion       string
	registryPattern string
	keychain        *KeychainProvider
}

// NewAWSCredentialProvider creates a provider that uses AWS credentials with the keychain
func NewAWSCredentialProvider(accessKey, secretKey, sessionToken, region, registryPattern string) *AWSCredentialProvider {
	if registryPattern == "" {
		registryPattern = "*.amazonaws.com"
	}
	return &AWSCredentialProvider{
		awsAccessKey:    accessKey,
		awsSecretKey:    secretKey,
		awsSessionToken: sessionToken,
		awsRegion:       region,
		registryPattern: registryPattern,
		keychain:        NewKeychainProvider(),
	}
}

func (p *AWSCredentialProvider) GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
	if !matchRegistryPattern(p.registryPattern, registry) {
		return nil, ErrNoCredentials
	}

	// Set AWS environment variables for keychain to use
	oldAccessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	oldSecretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	oldSessionToken := os.Getenv("AWS_SESSION_TOKEN")
	oldRegion := os.Getenv("AWS_REGION")

	os.Setenv("AWS_ACCESS_KEY_ID", p.awsAccessKey)
	os.Setenv("AWS_SECRET_ACCESS_KEY", p.awsSecretKey)
	if p.awsSessionToken != "" {
		os.Setenv("AWS_SESSION_TOKEN", p.awsSessionToken)
	}
	if p.awsRegion != "" {
		os.Setenv("AWS_REGION", p.awsRegion)
	}

	// Restore old values after getting credentials
	defer func() {
		os.Setenv("AWS_ACCESS_KEY_ID", oldAccessKey)
		os.Setenv("AWS_SECRET_ACCESS_KEY", oldSecretKey)
		os.Setenv("AWS_SESSION_TOKEN", oldSessionToken)
		os.Setenv("AWS_REGION", oldRegion)
	}()

	return p.keychain.GetCredentials(ctx, registry, scope)
}

func (p *AWSCredentialProvider) Name() string {
	return "aws-credentials"
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

// CredentialType represents different types of registry credentials
type CredentialType string

const (
	CredTypePublic  CredentialType = "public"
	CredTypeBasic   CredentialType = "basic"
	CredTypeAWS     CredentialType = "aws"
	CredTypeGCP     CredentialType = "gcp"
	CredTypeAzure   CredentialType = "azure"
	CredTypeToken   CredentialType = "token"
	CredTypeUnknown CredentialType = "unknown"
)

// DetectCredentialType determines the type of credentials based on the registry and credential keys
func DetectCredentialType(registry string, creds map[string]string) CredentialType {
	if len(creds) == 0 {
		return CredTypePublic
	}

	registry = strings.ToLower(registry)

	// Check for AWS credentials
	if _, hasAwsKey := creds["AWS_ACCESS_KEY_ID"]; hasAwsKey {
		if _, hasAwsSecret := creds["AWS_SECRET_ACCESS_KEY"]; hasAwsSecret {
			return CredTypeAWS
		}
	}

	// Check for GCP credentials
	if _, hasGcp := creds["GOOGLE_APPLICATION_CREDENTIALS"]; hasGcp {
		return CredTypeGCP
	}
	if _, hasGcpProject := creds["GCP_PROJECT_ID"]; hasGcpProject {
		return CredTypeGCP
	}
	if _, hasGcpToken := creds["GCP_ACCESS_TOKEN"]; hasGcpToken {
		return CredTypeGCP
	}

	// Check for Azure credentials
	if _, hasAzureClientId := creds["AZURE_CLIENT_ID"]; hasAzureClientId {
		if _, hasAzureSecret := creds["AZURE_CLIENT_SECRET"]; hasAzureSecret {
			return CredTypeAzure
		}
	}

	// Check for token-based auth (before basic auth)
	tokenKeys := []string{"NGC_API_KEY", "GITHUB_TOKEN", "DOCKERHUB_TOKEN"}
	for _, key := range tokenKeys {
		if _, hasToken := creds[key]; hasToken {
			return CredTypeToken
		}
	}

	// Check for basic auth (username/password)
	hasUsername := false
	hasPassword := false
	for key := range creds {
		keyUpper := strings.ToUpper(key)
		if strings.Contains(keyUpper, "USERNAME") {
			hasUsername = true
		}
		if strings.Contains(keyUpper, "PASSWORD") {
			hasPassword = true
		}
	}
	if hasUsername && hasPassword {
		return CredTypeBasic
	}

	// Detect based on registry
	if strings.Contains(registry, "ecr") || strings.Contains(registry, "amazonaws.com") {
		return CredTypeAWS
	}
	if strings.Contains(registry, "gcr.io") || strings.Contains(registry, "pkg.dev") {
		return CredTypeGCP
	}
	if strings.Contains(registry, "azurecr.io") {
		return CredTypeAzure
	}

	return CredTypeUnknown
}

// CreateProviderFromCredentials creates a CLIP-compatible credential provider from a credential map
// This is the main function that beta9 should use to create providers for CLIP
// Returns common.RegistryCredentialProvider
func CreateProviderFromCredentials(ctx context.Context, registry string, credType CredentialType, creds map[string]string) RegistryCredentialProvider {
	if len(creds) == 0 {
		return NewPublicOnlyProvider()
	}

	providerName := fmt.Sprintf("creds-%s", registry)
	registryLower := strings.ToLower(registry)

	switch credType {
	case CredTypeBasic:
		// Basic auth with username/password
		username := ""
		password := ""

		// Try different username keys (in order of specificity)
		usernameKeys := []string{"REGISTRY_USERNAME", "DOCKER_USERNAME", "USERNAME"}
		passwordKeys := []string{"REGISTRY_PASSWORD", "DOCKER_PASSWORD", "PASSWORD"}

		for _, key := range usernameKeys {
			if val, ok := creds[key]; ok && val != "" {
				username = val
				break
			}
		}
		for _, key := range passwordKeys {
			if val, ok := creds[key]; ok && val != "" {
				password = val
				break
			}
		}

		// Fallback: scan for any key containing USERNAME/PASSWORD
		if username == "" || password == "" {
			for key, value := range creds {
				keyUpper := strings.ToUpper(key)
				if username == "" && strings.Contains(keyUpper, "USERNAME") {
					username = value
				}
				if password == "" && strings.Contains(keyUpper, "PASSWORD") {
					password = value
				}
			}
		}

		if username != "" && password != "" {
			return NewStaticProviderWithName(providerName, map[string]*authn.AuthConfig{
				registry: {
					Username: username,
					Password: password,
				},
			})
		}
		return NewPublicOnlyProvider()

	case CredTypeAWS:
		// For AWS ECR, use the ECR provider which calls the API
		accessKey := creds["AWS_ACCESS_KEY_ID"]
		secretKey := creds["AWS_SECRET_ACCESS_KEY"]
		sessionToken := creds["AWS_SESSION_TOKEN"]
		region := creds["AWS_REGION"]

		if accessKey != "" && secretKey != "" && region != "" {
			log.Info().
				Str("registry", registry).
				Str("region", region).
				Msg("creating ECR provider with AWS credentials")

			return NewECRProvider(ECRProviderConfig{
				AWSAccessKey:    accessKey,
				AWSSecretKey:    secretKey,
				AWSSessionToken: sessionToken,
				AWSRegion:       region,
				RegistryPattern: registry, // Match specific registry
			})
		}
		return NewPublicOnlyProvider()

	case CredTypeGCP:
		// For GCP, check if we have an access token (simpler path)
		if token, ok := creds["GCP_ACCESS_TOKEN"]; ok && token != "" {
			// GCP uses oauth2accesstoken as username
			return NewStaticProviderWithName(providerName, map[string]*authn.AuthConfig{
				registry: {
					Username: "oauth2accesstoken",
					Password: token,
				},
			})
		}

		// Otherwise use keychain provider with env vars
		callback := func(ctx context.Context, reg string, scope string) (*authn.AuthConfig, error) {
			if reg != registry {
				return nil, ErrNoCredentials
			}

			// Set environment variables temporarily
			oldEnv := make(map[string]string)
			for key, value := range creds {
				oldEnv[key] = os.Getenv(key)
				os.Setenv(key, value)
			}

			// Restore environment after getting credentials
			defer func() {
				for key, oldValue := range oldEnv {
					os.Setenv(key, oldValue)
				}
			}()

			// Use keychain provider which handles GCR
			keychain := NewKeychainProvider()
			return keychain.GetCredentials(ctx, reg, scope)
		}

		return NewCallbackProviderWithName(providerName, callback)

	case CredTypeAzure:
		// For Azure, use keychain provider with env vars
		callback := func(ctx context.Context, reg string, scope string) (*authn.AuthConfig, error) {
			if reg != registry {
				return nil, ErrNoCredentials
			}

			// Set environment variables temporarily
			oldEnv := make(map[string]string)
			for key, value := range creds {
				oldEnv[key] = os.Getenv(key)
				os.Setenv(key, value)
			}

			// Restore environment after getting credentials
			defer func() {
				for key, oldValue := range oldEnv {
					os.Setenv(key, oldValue)
				}
			}()

			// Use keychain provider which handles ACR
			keychain := NewKeychainProvider()
			return keychain.GetCredentials(ctx, reg, scope)
		}

		return NewCallbackProviderWithName(providerName, callback)

	case CredTypeToken:
		// Handle registry-specific token formats
		
		// NGC (nvcr.io) - uses $oauthtoken as username
		if strings.Contains(registryLower, "nvcr.io") {
			if apiKey, ok := creds["NGC_API_KEY"]; ok && apiKey != "" {
				log.Debug().
					Str("registry", registry).
					Msg("creating NGC token provider")
				return NewStaticProviderWithName(providerName, map[string]*authn.AuthConfig{
					registry: {
						Username: "$oauthtoken",
						Password: apiKey,
					},
				})
			}
		}

		// GHCR (ghcr.io) - uses GitHub username and token
		if strings.Contains(registryLower, "ghcr.io") {
			githubUsername := creds["GITHUB_USERNAME"]
			githubToken := creds["GITHUB_TOKEN"]
			
			if githubToken != "" {
				// If no username provided, try common alternatives or use token as username
				if githubUsername == "" {
					githubUsername = creds["USERNAME"]
				}
				if githubUsername == "" {
					// Some setups use the token itself as username
					githubUsername = githubToken
				}
				
				log.Debug().
					Str("registry", registry).
					Bool("has_username", githubUsername != "").
					Msg("creating GHCR token provider")
				
				return NewStaticProviderWithName(providerName, map[string]*authn.AuthConfig{
					registry: {
						Username: githubUsername,
						Password: githubToken,
					},
				})
			}
		}

		// Docker Hub - uses DOCKERHUB_USERNAME and DOCKERHUB_PASSWORD or DOCKERHUB_TOKEN
		if strings.Contains(registryLower, "docker.io") || registry == "index.docker.io" || registry == "registry-1.docker.io" {
			dockerUsername := creds["DOCKERHUB_USERNAME"]
			dockerPassword := creds["DOCKERHUB_PASSWORD"]
			dockerToken := creds["DOCKERHUB_TOKEN"]
			
			// Prefer explicit Docker Hub credentials
			if dockerUsername != "" && dockerPassword != "" {
				log.Debug().
					Str("registry", registry).
					Msg("creating Docker Hub provider with username/password")
				return NewStaticProviderWithName(providerName, map[string]*authn.AuthConfig{
					registry: {
						Username: dockerUsername,
						Password: dockerPassword,
					},
				})
			}
			
			// Use token if provided
			if dockerToken != "" {
				if dockerUsername == "" {
					dockerUsername = creds["USERNAME"]
				}
				log.Debug().
					Str("registry", registry).
					Msg("creating Docker Hub provider with token")
				return NewStaticProviderWithName(providerName, map[string]*authn.AuthConfig{
					registry: {
						Username: dockerUsername,
						Password: dockerToken,
					},
				})
			}
		}

		// Generic token handling - try each token type
		tokenConfigs := []struct {
			key      string
			username string
		}{
			{"NGC_API_KEY", "$oauthtoken"},
			{"GITHUB_TOKEN", "oauth2accesstoken"},
			{"DOCKERHUB_TOKEN", ""},
			{"GCP_ACCESS_TOKEN", "oauth2accesstoken"},
		}

		for _, tc := range tokenConfigs {
			if token, ok := creds[tc.key]; ok && token != "" {
				username := tc.username
				if username == "" {
					// For tokens without a specific username, check for explicit username
					username = creds["USERNAME"]
					if username == "" {
						username = "oauth2accesstoken" // default
					}
				}
				
				log.Debug().
					Str("registry", registry).
					Str("token_key", tc.key).
					Str("username", username).
					Msg("creating token provider")
				
				return NewStaticProviderWithName(providerName, map[string]*authn.AuthConfig{
					registry: {
						Username: username,
						Password: token,
					},
				})
			}
		}
		
		return NewPublicOnlyProvider()

	default:
		return NewPublicOnlyProvider()
	}
}

// ParseCredentialsFromJSON parses credentials from JSON string or username:password format
// Returns structured credentials as a map
// Handles multiple formats:
// 1. Beta9 format: {"credentials": {...}, "registry": "...", "type": "..."}
// 2. Nested JSON strings: {"PASSWORD": "{\"AWS_ACCESS_KEY_ID\":\"...\"}"}
// 3. Flat JSON: {"USERNAME": "user", "PASSWORD": "pass"}
// 4. Legacy: "username:password"
func ParseCredentialsFromJSON(credStr string) (map[string]string, error) {
	if credStr == "" {
		return nil, nil
	}

	// Try to parse as structured object with interface{} values first (beta9 format)
	var structuredData map[string]interface{}
	if err := json.Unmarshal([]byte(credStr), &structuredData); err == nil {
		// Check if this is beta9 format with "credentials" object
		if credObj, hasCredentials := structuredData["credentials"]; hasCredentials {
			// Extract the credentials map
			if credMap, ok := credObj.(map[string]interface{}); ok {
				result := make(map[string]string)
				for k, v := range credMap {
					if strVal, ok := v.(string); ok {
						result[k] = strVal
					}
				}
				
				// Also include top-level string fields (registry, type, etc.)
				for k, v := range structuredData {
					if k == "credentials" {
						continue // Skip the credentials object itself
					}
					if strVal, ok := v.(string); ok {
						result[k] = strVal
					}
				}
				
				return result, nil
			}
		}
		
		// Otherwise, flatten all string values
		result := make(map[string]string)
		for k, v := range structuredData {
			if strVal, ok := v.(string); ok {
				result[k] = strVal
			}
		}
		
		// If we got some values, return them
		if len(result) > 0 {
			return result, nil
		}
	}

	// Try flat JSON format (map[string]string)
	var credMap map[string]string
	if err := json.Unmarshal([]byte(credStr), &credMap); err == nil {
		// Check if this is a nested structure where values are JSON strings
		result := make(map[string]string)
		
		// First, copy all existing keys
		for key, value := range credMap {
			result[key] = value
		}
		
		// Then try to extract nested JSON from string values
		for _, value := range credMap {
			extracted := extractNestedCredentials(value)
			for k, v := range extracted {
				// Don't overwrite existing keys
				if _, exists := result[k]; !exists {
					result[k] = v
				}
			}
		}
		
		return result, nil
	}

	// Try legacy username:password format
	parts := strings.SplitN(credStr, ":", 2)
	if len(parts) == 2 {
		return map[string]string{
			"USERNAME": parts[0],
			"PASSWORD": parts[1],
		}, nil
	}

	return nil, fmt.Errorf("unable to parse credentials: invalid format")
}

// extractNestedCredentials recursively extracts credentials from nested JSON strings
func extractNestedCredentials(jsonStr string) map[string]string {
	result := make(map[string]string)
	
	// Try to parse as a map with string values
	var strMap map[string]string
	if err := json.Unmarshal([]byte(jsonStr), &strMap); err == nil {
		for k, v := range strMap {
			result[k] = v
			// Recursively extract from nested values
			nested := extractNestedCredentials(v)
			for nk, nv := range nested {
				if _, exists := result[nk]; !exists {
					result[nk] = nv
				}
			}
		}
		return result
	}
	
	// Try to parse as a map with interface{} values (for nested objects)
	var interfaceMap map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &interfaceMap); err == nil {
		for k, v := range interfaceMap {
			switch val := v.(type) {
			case string:
				result[k] = val
				// Try to extract from this string
				nested := extractNestedCredentials(val)
				for nk, nv := range nested {
					if _, exists := result[nk]; !exists {
						result[nk] = nv
					}
				}
			case map[string]interface{}:
				// Handle nested maps (like "credentials" object)
				for nk, nv := range val {
					if strVal, ok := nv.(string); ok {
						result[nk] = strVal
					}
				}
			}
		}
		return result
	}
	
	return result
}

// CredentialsToProvider converts a credential map and registry to a CLIP-compatible provider
// This is a convenience function that auto-detects credential type and creates the appropriate provider
func CredentialsToProvider(ctx context.Context, registry string, creds map[string]string) RegistryCredentialProvider {
	if len(creds) == 0 {
		log.Debug().Str("registry", registry).Msg("no credentials provided, using public access")
		return NewPublicOnlyProvider()
	}

	credType := DetectCredentialType(registry, creds)
	log.Debug().
		Str("registry", registry).
		Str("cred_type", string(credType)).
		Int("cred_count", len(creds)).
		Msg("creating credential provider")

	return CreateProviderFromCredentials(ctx, registry, credType, creds)
}
