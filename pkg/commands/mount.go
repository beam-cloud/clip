package commands

import (
	"fmt"
	"os/exec"

	log "github.com/okteto/okteto/pkg/log"

	"github.com/beam-cloud/clip/pkg/clip"
	"github.com/spf13/cobra"
)

var mountOptions = &clip.MountOptions{}

var MountCmd = &cobra.Command{
	Use:   "mount",
	Short: "Mount an archive to a specified mount point",
	Run:   runMount,
}

func init() {
	MountCmd.Flags().StringVarP(&mountOptions.ArchivePath, "input", "i", "", "Archive file to mount")
	MountCmd.Flags().StringVarP(&mountOptions.MountPoint, "mountpoint", "m", "", "Directory to mount the archive")
	MountCmd.Flags().BoolVarP(&mountOptions.Verbose, "verbose", "v", false, "Verbose output")
	MountCmd.Flags().StringVarP(&mountOptions.CachePath, "cache", "c", "", "Cache clip locally")
	MountCmd.MarkFlagRequired("input")
	MountCmd.MarkFlagRequired("mountpoint")
}

func forceUnmount() {
	unmountCommand := exec.Command("umount", "-f", mountOptions.MountPoint)
	unmountCommand.Run()
}

func runMount(cmd *cobra.Command, args []string) {
	forceUnmount() // Force unmount the file system if it's already mounted

	startServer, serverError, err := clip.MountArchive(*mountOptions)
	if err != nil {
		log.Fatalf("Failed to mount archive: %v", err)
	}

	err = startServer()
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	log.Success(fmt.Sprintf("Mounted %s to %s successfully.", mountOptions.ArchivePath, mountOptions.MountPoint))
	for err := range serverError {
		if err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}

}
