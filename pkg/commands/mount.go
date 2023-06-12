package commands

import (
	"fmt"
	"os/exec"

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

	ca := archive.NewClipArchiver()
	metadata, err := ca.ExtractMetadata(mountOptions.archivePath)
	if err != nil {
		log.Fatalf("invalid archive: %v", err)
	}

	header := metadata.Header
	var storageType string = ""
	var storageOpts storage.ClipStorageOpts

	// This a remote archive, so we have to load that particular storage implementation
	if header.StorageInfoLength > 0 {
	} else {
		storageType = "local"
		storageOpts = storage.LocalClipStorageOpts{}
	}

	s, err := storage.NewClipStorage(metadata, storageType, storageOpts)
	if err != nil {
		log.Fatalf("Could not load storage: %v", err)
	}

	clipfs := clipfs.NewFileSystem(s)
	root, _ := clipfs.Root()

	fsOptions := &fs.Options{}
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
