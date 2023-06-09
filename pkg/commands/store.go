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

var StoreLocalCmd = &cobra.Command{
	Use:   "local",
	Short: "Generate an RCLIP archive backed by a local file.",
	RunE:  runStoreLocal,
}

type StoreS3Options struct {
	InputFile  string
	AccessKey  string
	SecretKey  string
	Bucket     string
	OutputFile string
}

type StoreLocalOptions struct {
	InputFile  string
	OutputFile string
}

var storeS3Opts = &StoreS3Options{}
var storeLocalOpts = &StoreLocalOptions{}

func init() {
	StoreCmd.AddCommand(StoreS3Cmd)
	StoreCmd.AddCommand(StoreLocalCmd)

	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.InputFile, "input", "i", "", "Input CLIP archive path")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.OutputFile, "output", "o", "", "Output RCLIP archive path")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.AccessKey, "access-key", "a", "", "S3 access key")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.SecretKey, "secret-key", "s", "", "S3 secret key")
	StoreS3Cmd.Flags().StringVarP(&storeS3Opts.Bucket, "bucket", "b", "", "S3 bucket")
	StoreS3Cmd.MarkFlagRequired("input")
	StoreS3Cmd.MarkFlagRequired("output")

	StoreLocalCmd.Flags().StringVarP(&storeLocalOpts.InputFile, "input", "i", "", "Input CLIP archive path")
	StoreLocalCmd.Flags().StringVarP(&storeLocalOpts.OutputFile, "output", "o", "", "Output RCLIP archive path")
	StoreLocalCmd.MarkFlagRequired("input")
	StoreLocalCmd.MarkFlagRequired("output")
}

func runStoreS3(cmd *cobra.Command, args []string) error {
	storageInfo := &archive.S3StorageInfo{Bucket: storeS3Opts.Bucket}
	a, err := archive.NewRClipArchiver(storageInfo)
	if err != nil {
		return err
	}

	err = a.Create(archive.ClipArchiverOptions{ArchivePath: storeS3Opts.InputFile})
	if err != nil {
		return err
	}

	return nil
}

func runStoreLocal(cmd *cobra.Command, args []string) error {
	storageInfo := &archive.LocalStorageInfo{Path: storeLocalOpts.InputFile}
	a, err := archive.NewRClipArchiver(storageInfo)
	if err != nil {
		return err
	}

	err = a.Create(archive.ClipArchiverOptions{ArchivePath: storeLocalOpts.InputFile})
	if err != nil {
		return err
	}

	return nil
}
