package commands

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/beam-cloud/clip/pkg/archive"
	clipfs "github.com/beam-cloud/clip/pkg/clipfs"
	storage "github.com/beam-cloud/clip/pkg/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	log "github.com/okteto/okteto/pkg/log"
	"github.com/spf13/cobra"
)

type MountOptions struct {
	archivePath string
	mountPoint  string
	Verbose     bool
}

var mountOptions MountOptions

var MountCmd = &cobra.Command{
	Use:   "mount",
	Short: "Mount an archive to a specified mount point",
	Run:   runMount,
}

func init() {
	MountCmd.Flags().StringVarP(&mountOptions.archivePath, "input", "i", "", "Archive file to mount")
	MountCmd.Flags().StringVarP(&mountOptions.mountPoint, "mountpoint", "m", "", "Directory to mount the archive")
	MountCmd.Flags().BoolVarP(&mountOptions.Verbose, "verbose", "v", false, "Verbose output")
	MountCmd.MarkFlagRequired("input")
	MountCmd.MarkFlagRequired("mountpoint")
}

func forceUnmount() {
	unmountCommand := exec.Command("umount", "-f", mountOptions.mountPoint)
	unmountCommand.Run()
}

func runMount(cmd *cobra.Command, args []string) {
	log.Information(fmt.Sprintf("Mounting archive: %s to %s", mountOptions.archivePath, mountOptions.mountPoint))

	forceUnmount() // Force unmount the file system if it's already mounted

	// Check if the mount point directory exists, if not, create it
	if _, err := os.Stat(mountOptions.mountPoint); os.IsNotExist(err) {
		err = os.MkdirAll(mountOptions.mountPoint, 0755)
		if err != nil {
			log.Fatalf("Failed to create mount point directory: %v", err)
		}
		log.Information("Mount point directory created.")
	}

	ca := archive.NewClipArchiver()
	metadata, err := ca.ExtractMetadata(mountOptions.archivePath)
	if err != nil {
		log.Fatalf("invalid archive: %v", err)
	}

	s, err := storage.NewClipStorage(mountOptions.archivePath, metadata)
	if err != nil {
		log.Fatalf("Could not load storage: %v", err)
	}

	clipfs, err := clipfs.NewFileSystem(s, mountOptions.Verbose)
	if err != nil {
		log.Fatalf("Could not create filesystem: %v", err)
	}

	root, _ := clipfs.Root()

	attrTimeout := time.Second * 60
	entryTimeout := time.Second * 60
	fsOptions := &fs.Options{
		AttrTimeout:  &attrTimeout,
		EntryTimeout: &entryTimeout,
	}
	server, err := fuse.NewServer(fs.NewNodeFS(root, fsOptions), mountOptions.mountPoint, &fuse.MountOptions{})
	if err != nil {
		log.Fatalf("Could not create server: %v", err)
	}

	go server.Serve() // Run the FUSE server in the background

	if err := server.WaitMount(); err != nil {
		log.Fatalf("Failed to mount: %v", err)
	}

	log.Success("Mounted successfully.")

	// Block until the FUSE server stops, this will happen when the filesystem is unmounted.
	server.Wait()
}
