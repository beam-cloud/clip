# Registry Authentication for CLIP

This package provides a pluggable, runtime-based authentication system for OCI registry access in CLIP. It enables secure, flexible credential management without persisting secrets in archive metadata.

## Design Principles

1. **Auth-Agnostic Archives**: `.clip` files contain *what* to fetch (manifests, layers, digests, registry FQDN), not *how* to fetch it
2. **Runtime-Supplied Credentials**: Callers provide credentials every time they perform networked registry operations
3. **Pluggable Providers**: CLI, library, or k8s operators can choose how to obtain credentials
4. **Short-Lived Token Friendly**: Supports token refresh for long-lived mounts (ECR, GCR, etc.)
5. **Backwards Compatible**: Existing public-registry users aren't affected; legacy base64 auth config is deprecated gracefully

## Core Interface

```go
type RegistryCredentialProvider interface {
    // GetCredentials returns authentication configuration for a given registry
    GetCredentials(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error)
    
    // Name returns a human-readable name for this provider (for logging/debugging)
    Name() string
}
```

### Parameters

- **ctx**: Context for cancellation and timeouts
- **registry**: Registry hostname (e.g., `"ghcr.io"`, `"registry-1.docker.io"`, `"123456789.dkr.ecr.us-east-1.amazonaws.com"`)
- **scope**: Optional repository path for per-repo tokens (e.g., `"beam-cloud/clip"`)

### Return Values

- **\*authn.AuthConfig**: Credentials if available
- **error**: `ErrNoCredentials` if unavailable, or another error if lookup failed

## Built-in Providers

### 1. PublicOnlyProvider

Always returns `ErrNoCredentials`, forcing anonymous/public access.

```go
provider := registryauth.NewPublicOnlyProvider()
```

**Use case**: Testing, public-only registries

### 2. StaticProvider

Returns pre-configured credentials for specific registries.

```go
provider := registryauth.NewStaticProvider(map[string]*authn.AuthConfig{
    "ghcr.io": {
        Username: "myuser",
        Password: "mytoken",
    },
    "registry-1.docker.io": {
        Username: "dockeruser",
        Password: "dockerpass",
    },
})
```

**Use case**: Embedded applications, testing, simple single-credential setups

### 3. DockerConfigProvider

Reads credentials from Docker's `config.json`.

```go
// Use default location (~/.docker/config.json or $DOCKER_CONFIG)
provider := registryauth.NewDockerConfigProvider("")

// Or specify custom path
provider := registryauth.NewDockerConfigProvider("/path/to/config.json")
```

**Use case**: Development environments, CI/CD with Docker login

### 4. EnvProvider

Reads credentials from environment variables.

#### Format 1: Individual environment variables

```bash
export CLIP_REGISTRY_USER_GHCR_IO="myuser"
export CLIP_REGISTRY_PASS_GHCR_IO="mytoken"

# For registries with dots/dashes, they're converted to underscores
export CLIP_REGISTRY_USER_123456789_DKR_ECR_US_EAST_1_AMAZONAWS_COM="AWS"
export CLIP_REGISTRY_PASS_123456789_DKR_ECR_US_EAST_1_AMAZONAWS_COM="..."
```

#### Format 2: JSON

```bash
export CLIP_OCI_AUTH='{
  "ghcr.io": {
    "username": "myuser",
    "password": "mytoken"
  },
  "gcr.io": {
    "token": "ya29...."
  }
}'
```

```go
provider := registryauth.NewEnvProvider()
```

**Use case**: Kubernetes secrets projected as environment variables, CI/CD, serverless environments

### 5. KeychainProvider

Wraps go-containerregistry's default keychain, automatically supporting:
- Docker config
- GCR (Google Container Registry)
- ECR (Amazon Elastic Container Registry)
- Other standard auth methods

```go
provider := registryauth.NewKeychainProvider()
```

**Use case**: General-purpose, works in most environments without configuration

### 6. ChainedProvider

Tries multiple providers in order until one succeeds.

```go
provider := registryauth.NewChainedProvider(
    registryauth.NewEnvProvider(),
    registryauth.NewDockerConfigProvider(""),
    registryauth.NewKeychainProvider(),
)
```

**Use case**: Production environments where credentials may come from multiple sources

### 7. CallbackProvider

Allows custom credential resolution logic.

```go
provider := registryauth.NewCallbackProvider(func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
    // Your custom logic here
    if registry == "my-registry.io" {
        return &authn.AuthConfig{
            Username: "computed-user",
            Password: fetchTokenFromVault(ctx),
        }, nil
    }
    return nil, registryauth.ErrNoCredentials
})
```

**Use case**: Integration with custom IAM systems, secret management services (Vault, AWS Secrets Manager, etc.)

### 8. CachingProvider

Wraps another provider with caching and TTL support (useful for short-lived tokens).

```go
baseProvider := registryauth.NewCallbackProvider(fetchTokenFromECR)
provider := registryauth.NewCachingProvider(baseProvider, 15*time.Minute)
```

**Use case**: ECR, GCR, or any provider that issues short-lived tokens

## Usage Examples

### Example 1: Creating an OCI Index with Authentication

```go
import (
    "context"
    "github.com/beam-cloud/clip/pkg/clip"
    "github.com/beam-cloud/clip/pkg/registryauth"
)

// Create a credential provider
credProvider := registryauth.NewStaticProvider(map[string]*authn.AuthConfig{
    "ghcr.io": {
        Username: "myuser",
        Password: "ghp_mytoken",
    },
})

// Create OCI index with authentication
err := clip.CreateFromOCIImage(context.Background(), clip.CreateFromOCIImageOptions{
    ImageRef:      "ghcr.io/myorg/private-image:latest",
    OutputPath:    "archive.clip",
    CheckpointMiB: 2,
    CredProvider:  credProvider, // Pass the provider
})
```

### Example 2: Mounting with Authentication

```go
import (
    "github.com/beam-cloud/clip/pkg/clip"
    "github.com/beam-cloud/clip/pkg/registryauth"
)

// Create a credential provider (e.g., chained for flexibility)
credProvider := registryauth.NewChainedProvider(
    registryauth.NewEnvProvider(),
    registryauth.NewDockerConfigProvider(""),
    registryauth.NewKeychainProvider(),
)

// Mount with authentication
startServer, serverErr, server, err := clip.MountArchive(clip.MountOptions{
    ArchivePath:          "archive.clip",
    MountPoint:           "/mnt/clip",
    RegistryCredProvider: credProvider, // Pass the provider
    UseCheckpoints:       true,
})
```

### Example 3: Using Default Provider

The `DefaultProvider()` function returns a sensible default chain that works in most environments:

```go
provider := registryauth.DefaultProvider()
// Equivalent to:
// NewChainedProvider(
//     NewEnvProvider(),
//     NewDockerConfigProvider(""),
//     NewKeychainProvider(),
// )
```

### Example 4: ECR with Token Refresh

```go
import (
    "context"
    "time"
    "github.com/beam-cloud/clip/pkg/registryauth"
    // ... ECR SDK imports
)

// Create a callback that fetches ECR tokens
ecrCallback := func(ctx context.Context, registry string, scope string) (*authn.AuthConfig, error) {
    if !strings.Contains(registry, ".dkr.ecr.") {
        return nil, registryauth.ErrNoCredentials
    }
    
    // Fetch token from ECR
    token, err := getECRAuthToken(ctx, registry)
    if err != nil {
        return nil, err
    }
    
    return &authn.AuthConfig{
        Username: "AWS",
        Password: token,
    }, nil
}

// Wrap with caching (ECR tokens last 12 hours)
provider := registryauth.NewCachingProvider(
    registryauth.NewCallbackProvider(ecrCallback),
    11*time.Hour, // Refresh before expiry
)
```

## Migration from Legacy AuthConfig

The old `AuthConfig` field (base64-encoded credentials) is deprecated but still supported:

```go
// OLD (deprecated):
clip.CreateFromOCIImageOptions{
    ImageRef:   "ghcr.io/myorg/image:latest",
    AuthConfig: "base64encodedcreds...", // DEPRECATED
}

// NEW (recommended):
provider := registryauth.NewStaticProvider(map[string]*authn.AuthConfig{
    "ghcr.io": {Username: "user", Password: "token"},
})
clip.CreateFromOCIImageOptions{
    ImageRef:     "ghcr.io/myorg/image:latest",
    CredProvider: provider, // Use this instead
}
```

If you pass a legacy `AuthConfig`, CLIP will:
1. Parse it and create a temporary `StaticProvider`
2. Log a deprecation warning
3. Use it for that operation only (not persisted)

## Security Considerations

1. **Never Log Credentials**: Providers must never log passwords or tokens
2. **No Persistence**: Credentials are never written to `.clip` files or disk metadata
3. **Sanitized Errors**: Auth failures return generic errors without exposing credentials
4. **Context Cancellation**: Always respect context for timeout/cancellation

## Architecture

### Archive Time (Building .clip)

```
┌────────────┐
│   Caller   │
│  (CLI/API) │
└──────┬─────┘
       │ Provides CredProvider
       ▼
┌────────────────────┐
│   OCI Indexer      │
│  ┌──────────────┐  │
│  │CredProvider  │  │ GetCredentials(registry, scope)
│  │.GetCreds()   ├──┤────────────────────────────────┐
│  └──────────────┘  │                                │
│                    │                                ▼
│  ┌──────────────┐  │                      ┌─────────────────┐
│  │ Fetch Image  │  │                      │ Auth Provider   │
│  │   Manifest   │◄─┼──────────────────────┤  (env/docker/   │
│  └──────────────┘  │     Inject auth      │   keychain)     │
│                    │                      └─────────────────┘
│  ┌──────────────┐  │
│  │ Build Index  │  │
│  │ (no secrets) │  │
│  └──────────────┘  │
└────────┬───────────┘
         │
         ▼
    archive.clip
    (metadata only,
     no credentials!)
```

### Mount Time (Reading from .clip)

```
┌────────────┐
│   Caller   │
│ (FUSE app) │
└──────┬─────┘
       │ Provides CredProvider
       ▼
┌────────────────────┐
│  CLIP Filesystem   │
│  ┌──────────────┐  │
│  │ OCIStorage   │  │
│  │ CredProvider │  │
│  └──────┬───────┘  │
│         │          │
│         │ Read needed layer
│         ▼          │
│  ┌──────────────┐  │
│  │ GetCreds()   ├──┤────────────────────────────────┐
│  └──────────────┘  │                                │
│                    │                                ▼
│  ┌──────────────┐  │                      ┌─────────────────┐
│  │ Fetch Blob   │  │                      │ Auth Provider   │
│  │ from Registry│◄─┼──────────────────────┤ (re-auth if     │
│  └──────────────┘  │     Inject auth      │  token expired) │
│                    │                      └─────────────────┘
└────────────────────┘
```

## Testing

The package includes comprehensive tests for all providers:

```bash
go test -v ./pkg/registryauth/...
```

### Test Coverage

- ✅ PublicOnlyProvider
- ✅ StaticProvider (single/multiple registries)
- ✅ DockerConfigProvider (with Docker Hub variants)
- ✅ EnvProvider (individual vars + JSON format)
- ✅ ChainedProvider (multiple providers, fallback)
- ✅ CallbackProvider (custom logic)
- ✅ CachingProvider (TTL, expiration, scope separation)
- ✅ DefaultProvider
- ✅ Legacy base64 AuthConfig parsing

## FAQ

### Q: Do old .clip files work with the new auth system?

**Yes!** The archive format hasn't changed. Old `.clip` files (created weeks/months ago) work with new credentials because archives only store addresses, not secrets.

### Q: What happens if credentials expire during a mount?

Providers are called per-request (or with caching if using `CachingProvider`). If a token expires, the next request will call the provider again to fetch fresh credentials.

### Q: Can I use different providers for indexing vs mounting?

Yes! You can use a `StaticProvider` during indexing and a `CallbackProvider` (that fetches from your IAM) during mounting.

### Q: How do I test private registry access?

Use `StaticProvider` or `DockerConfigProvider` with test credentials. For integration tests, use testcontainers with a local registry.

### Q: What if I don't provide a CredProvider?

CLIP will use the `DefaultProvider()` chain, which tries env vars → Docker config → keychain. This works for most cases.

## Implementation Notes

### Why interface{} in Public API?

The `CredProvider` field in `CreateFromOCIImageOptions` and `MountOptions` is `interface{}` to avoid import cycles and keep the public API simple. Internally, it's type-asserted to `RegistryCredentialProvider`.

### Thread Safety

All built-in providers are thread-safe. If you implement a custom provider, ensure your implementation is safe for concurrent use.

### Performance

- Credential lookups should be fast (< 10ms for local lookups)
- Use `CachingProvider` for expensive operations (network calls, crypto)
- Keychain operations may involve disk I/O (Docker config parsing)
