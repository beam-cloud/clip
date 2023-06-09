package main

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/beam-cloud/clip/pkg/commands"
	log "github.com/okteto/okteto/pkg/log"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "clip",
		Short: "A tool to create, extract, and mount clip archives",
	}

	rootCmd.AddCommand(commands.CreateCmd)
	rootCmd.AddCommand(commands.ExtractCmd)
	rootCmd.AddCommand(commands.StoreCmd)
	rootCmd.AddCommand(commands.MountCmd)

	// Setup signal catching
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)

	go func() {
		<-sigs
		log.StopSpinner()
		log.Println("Exiting. 👋")
		os.Exit(1)
	}()

	// If an error occurs, it will appear here.
	if err := rootCmd.Execute(); err != nil {
		log.StopSpinner()
		log.Fail(fmt.Sprintf("Failed to execute command: %v", err))
		os.Exit(1)
	}
}
