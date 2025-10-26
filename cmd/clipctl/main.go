package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/beam-cloud/clip/pkg/metrics"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	defaultCheckpointMiB = 2
	defaultCacheDir      = "/var/cache/clip"
	defaultBaseDir       = "/var/lib/clip"
)

func main() {
	// Setup logging
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	
	switch command {
	case "index":
		indexCommand()
	case "index-layout":
		indexLayoutCommand()
	case "mount":
		mountCommand()
	case "umount", "unmount":
		umountCommand()
	case "metrics":
		metricsCommand()
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `clipctl - Clip v2 OCI Image Management Tool

Usage:
  clipctl <command> [options]

Commands:
  index        Build metadata-only index file from OCI image
  index-layout Build metadata-only index file from local OCI layout
  mount        Mount OCI image as rootfs for containers
  umount       Unmount and cleanup container rootfs
  metrics      Show performance metrics

Examples:
  # Build index from OCI image
  clipctl index --image docker.io/library/python:3.12 --out /var/lib/clip/indices/python:3.12.clip

  # Build index from local OCI layout (for buildah/skopeo workflows)
  clipctl index-layout --layout /tmp/ubuntu --tag latest --out ubuntu.clip

  # Mount image for container
  clipctl mount --image docker.io/library/python:3.12 --cid mycontainer

  # Unmount container
  clipctl umount --cid mycontainer

Environment Variables:
  CLIP_REGISTRY_AUTH     Authentication mode (auto, none)
  CLIP_CHECKPOINT_MIB    Checkpoint interval in MiB (default: 2)
  CLIP_CACHE_DIR         Cache directory (default: /var/cache/clip)
  CLIP_BASE_DIR          Base directory (default: /var/lib/clip)

`)
}

func indexCommand() {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	
	var (
		imageRef       = fs.String("image", "", "OCI image reference (required)")
		outputPath     = fs.String("out", "", "Output .clip file path (required)")
		registryURL    = fs.String("registry", "", "Registry URL (auto-detected if not specified)")
		authConfigPath = fs.String("auth-config", "", "Path to Docker config.json (default: ~/.docker/config.json)")
		checkpointMiB  = fs.Int("checkpoint-mib", getEnvInt("CLIP_CHECKPOINT_MIB", defaultCheckpointMiB), "Checkpoint interval in MiB")
		verbose        = fs.Bool("verbose", false, "Verbose logging")
	)
	
	fs.Parse(os.Args[2:])
	
	if *imageRef == "" || *outputPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --image and --out are required\n\n")
		fs.Usage()
		os.Exit(1)
	}
	
	if *verbose {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	
	// Set auth config path default
	if *authConfigPath == "" {
		if homeDir, err := os.UserHomeDir(); err == nil {
			*authConfigPath = filepath.Join(homeDir, ".docker", "config.json")
		}
	}
	
	log.Info().Msgf("indexing OCI image: %s", *imageRef)
	log.Info().Msgf("checkpoint interval: %d MiB", *checkpointMiB)
	
	// Create archiver and index the image
	archiver := clip.NewClipArchiver()
	
	ctx := context.Background()
	err := archiver.CreateFromOCI(ctx, *imageRef, *outputPath, *registryURL, *authConfigPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create index from OCI image")
	}
	
	// Get file size for reporting
	if stat, err := os.Stat(*outputPath); err == nil {
		log.Info().Msgf("index created successfully: %s (size: %d bytes)", *outputPath, stat.Size())
	} else {
		log.Info().Msgf("index created successfully: %s", *outputPath)
	}
}

func indexLayoutCommand() {
	fs := flag.NewFlagSet("index-layout", flag.ExitOnError)
	
	var (
		layoutPath     = fs.String("layout", "", "OCI layout directory path (required)")
		tag           = fs.String("tag", "latest", "Image tag to index")
		outputPath    = fs.String("out", "", "Output .clip file path (required)")
		checkpointMiB = fs.Int("checkpoint-mib", getEnvInt("CLIP_CHECKPOINT_MIB", defaultCheckpointMiB), "Checkpoint interval in MiB")
		verbose       = fs.Bool("verbose", false, "Verbose logging")
	)
	
	fs.Parse(os.Args[2:])
	
	if *layoutPath == "" || *outputPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --layout and --out are required\n\n")
		fs.Usage()
		os.Exit(1)
	}
	
	if *verbose {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	
	log.Info().Msgf("indexing OCI layout: %s:%s", *layoutPath, *tag)
	log.Info().Msgf("checkpoint interval: %d MiB", *checkpointMiB)
	
	// Create archiver and index the OCI layout
	archiver := clip.NewClipArchiver()
	
	ctx := context.Background()
	err := archiver.CreateFromOCILayout(ctx, *layoutPath, *tag, *outputPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create index from OCI layout")
	}
	
	// Get file size for reporting
	if stat, err := os.Stat(*outputPath); err == nil {
		log.Info().Msgf("index created successfully: %s (size: %d bytes)", *outputPath, stat.Size())
	} else {
		log.Info().Msgf("index created successfully: %s", *outputPath)
	}
}

func mountCommand() {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	
	var (
		imageRef    = fs.String("image", "", "OCI image reference (required)")
		containerID = fs.String("cid", "", "Container ID (required)")
		clipFile    = fs.String("clip", "", "Path to .clip file (if not specified, will be auto-generated)")
		baseDir     = fs.String("base-dir", getEnvString("CLIP_BASE_DIR", defaultBaseDir), "Base directory for clip operations")
		verbose     = fs.Bool("verbose", false, "Verbose logging")
	)
	
	fs.Parse(os.Args[2:])
	
	if (*imageRef == "" && *clipFile == "") || *containerID == "" {
		fmt.Fprintf(os.Stderr, "Error: (--image or --clip) and --cid are required\n\n")
		fs.Usage()
		os.Exit(1)
	}
	
	if *verbose {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	
	// Create overlay manager
	overlayManager := clip.NewOverlayManager(*baseDir)
	
	var archivePath string
	
	if *clipFile != "" {
		// Use existing clip file
		archivePath = *clipFile
	} else {
		// Generate clip file from OCI image
		archivePath = filepath.Join(*baseDir, "indices", sanitizeImageRef(*imageRef)+".clip")
		
		// Create indices directory
		if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
			log.Fatal().Err(err).Msg("failed to create indices directory")
		}
		
		// Check if clip file already exists
		if _, err := os.Stat(archivePath); os.IsNotExist(err) {
			log.Info().Msgf("generating clip file from OCI image: %s", *imageRef)
			
			archiver := clip.NewClipArchiver()
			ctx := context.Background()
			
			// Get auth config path
			authConfigPath := ""
			if homeDir, err := os.UserHomeDir(); err == nil {
				authConfigPath = filepath.Join(homeDir, ".docker", "config.json")
			}
			
			err := archiver.CreateFromOCI(ctx, *imageRef, archivePath, "", authConfigPath)
			if err != nil {
				log.Fatal().Err(err).Msg("failed to create clip file from OCI image")
			}
		} else {
			log.Info().Msgf("using existing clip file: %s", archivePath)
		}
	}
	
	// Generate image digest for stable paths
	imageDigest := generateImageDigest(*imageRef)
	
	// Mount read-only FUSE filesystem
	roMountPath, err := overlayManager.MountReadOnly(archivePath, imageDigest)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to mount read-only filesystem")
	}
	
	// Setup overlay mount
	rootfsPath := overlayManager.GetRootfsPath(*containerID)
	overlayOpts := clip.OverlayMountOptions{
		ImageDigest: imageDigest,
		ContainerID: *containerID,
		ReadOnlyDir: roMountPath,
		RootfsPath:  rootfsPath,
	}
	
	err = overlayManager.SetupOverlayMount(overlayOpts)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to setup overlay mount")
	}
	
	// Output the rootfs path for container runtime
	fmt.Println(rootfsPath)
	log.Info().Msgf("container rootfs ready at: %s", rootfsPath)
}

func umountCommand() {
	fs := flag.NewFlagSet("umount", flag.ExitOnError)
	
	var (
		containerID = fs.String("cid", "", "Container ID (required)")
		baseDir     = fs.String("base-dir", getEnvString("CLIP_BASE_DIR", defaultBaseDir), "Base directory for clip operations")
		verbose     = fs.Bool("verbose", false, "Verbose logging")
	)
	
	fs.Parse(os.Args[2:])
	
	if *containerID == "" {
		fmt.Fprintf(os.Stderr, "Error: --cid is required\n\n")
		fs.Usage()
		os.Exit(1)
	}
	
	if *verbose {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	
	// Create overlay manager
	overlayManager := clip.NewOverlayManager(*baseDir)
	
	// Cleanup overlay mount
	err := overlayManager.CleanupOverlayMount(*containerID)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to cleanup overlay mount")
	}
	
	log.Info().Msgf("container %s unmounted and cleaned up", *containerID)
}

func metricsCommand() {
	fs := flag.NewFlagSet("metrics", flag.ExitOnError)
	
	var (
		format  = fs.String("format", "json", "Output format (json, prometheus, summary)")
		serve   = fs.Bool("serve", false, "Start HTTP metrics server")
		port    = fs.String("port", "8080", "HTTP server port")
		verbose = fs.Bool("verbose", false, "Verbose logging")
	)
	
	fs.Parse(os.Args[2:])
	
	if *verbose {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	
	if *serve {
		// Start HTTP metrics server
		http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			metricsData := metrics.GlobalMetrics.GetPrometheusMetrics()
			
			switch r.URL.Query().Get("format") {
			case "prometheus":
				w.Header().Set("Content-Type", "text/plain")
				for key, value := range metricsData {
					fmt.Fprintf(w, "%s %v\n", key, value)
				}
			default:
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(metricsData)
			}
		})
		
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "OK")
		})
		
		log.Info().Msgf("starting metrics server on port %s", *port)
		log.Info().Msg("endpoints: /metrics, /health")
		
		if err := http.ListenAndServe(":"+*port, nil); err != nil {
			log.Fatal().Err(err).Msg("failed to start metrics server")
		}
	} else {
		// One-time metrics output
		switch *format {
		case "prometheus":
			metricsData := metrics.GlobalMetrics.GetPrometheusMetrics()
			for key, value := range metricsData {
				fmt.Printf("%s %v\n", key, value)
			}
		case "summary":
			metrics.LogMetricsSummary()
		default: // json
			metricsData := metrics.GlobalMetrics.GetPrometheusMetrics()
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			encoder.Encode(metricsData)
		}
	}
}

// Helper functions

func getEnvString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed := parseInt(value); parsed > 0 {
			return parsed
		}
	}
	return defaultValue
}

func parseInt(s string) int {
	var result int
	fmt.Sscanf(s, "%d", &result)
	return result
}

func sanitizeImageRef(imageRef string) string {
	// Replace invalid filesystem characters
	sanitized := strings.ReplaceAll(imageRef, ":", "_")
	sanitized = strings.ReplaceAll(sanitized, "/", "_")
	sanitized = strings.ReplaceAll(sanitized, "@", "_")
	return sanitized
}

func generateImageDigest(imageRef string) string {
	// For now, use sanitized image ref as digest
	// In a full implementation, we'd compute the actual manifest digest
	return sanitizeImageRef(imageRef)
}