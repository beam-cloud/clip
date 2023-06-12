package commands

import (
	"github.com/beam-cloud/clip/pkg/archive"
	"github.com/spf13/cobra"
)

type StoreCmdOptions struct {
	InputFile  string
	RemotePath string
}

var StoreCmd = &cobra.Command{
	Use:   "store",
	Short: "Store a CLIP archive elsewhere and create an RCLIP archive",
}

var StoreS3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Generate an RCLIP archive backed by s3.",
	RunE:  runStoreS3,
}

type StoreS3Options struct {
	ArchivePath string
	AccessKey   string
	SecretKey   string
	Bucket      string
	OutputFile  string
}

var storeS3Opts = &StoreS3Options{}

func init() {
	StoreCmd.AddCommand(StoreS3Cmd)

	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.ArchivePath, "input", "i", "", "Input CLIP archive path")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.OutputFile, "output", "o", "", "Output RCLIP archive path")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.AccessKey, "access-key", "a", "", "S3 access key")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.SecretKey, "secret-key", "s", "", "S3 secret key")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.Bucket, "bucket", "b", "", "S3 bucket")
	StoreS3Cmd.MarkFlagRequired("input")
	StoreS3Cmd.MarkFlagRequired("output")

}

func runStoreS3(cmd *cobra.Command, args []string) error {
	storageInfo := &archive.S3StorageInfo{Bucket: storeS3Opts.Bucket}
	a, err := archive.NewRClipArchiver(storageInfo)
	if err != nil {
		return err
	}

	err = a.Create(archive.ClipArchiverOptions{ArchivePath: storeS3Opts.ArchivePath})
	if err != nil {
		return err
	}

	return nil
}
