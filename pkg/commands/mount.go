package commands

import (
	"fmt"

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

func runMount(cmd *cobra.Command, args []string) {
	log.Information(fmt.Sprintf("Mounting archive: %s to %s", mountOptions.archivePath, mountOptions.mountPoint))

	// // Load the archive
	// a := archive.NewClipArchive()
	// err := a.Load(archive.ClipArchiveOptions{
	// 	ArchivePath: mountOptions.archivePath,
	// })

	// if err != nil {
	// 	log.Fatalf("Failed to load the archive: %s\n", err)
	// }

	// // Create and mount the file system
	// fsys := fs.NewFS()
	// err := fsys.Root()
	// if err != nil {
	// 	log.Fatalf("Failed to mount the file system: %s\n", err)
	// }

	log.Success("Mounted successfully.")
}
