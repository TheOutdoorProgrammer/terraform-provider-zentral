package main

import (
	"context"
	"flag"
	"log"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/zentralopensource/terraform-provider-zentral/internal/provider"
	"github.com/zentralopensource/terraform-provider-zentral/internal/telemetry"
)

var (
	version string = "dev"
	commit  string = "unknown"
)

// Generate the Terraform provider documentation using `tfplugindocs`:
//go:generate go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs

func main() {
	var debug bool

	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	shutdown, err := telemetry.Initialize(context.Background(), version)
	if err != nil {
		log.Fatal(err.Error())
	}

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/theoutdoorprogrammer/zentral",
		Debug:   debug,
	}

	err = providerserver.Serve(context.Background(), provider.New(version), opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shutdownErr := shutdown(ctx); shutdownErr != nil {
		log.Print(shutdownErr.Error())
	}

	if err != nil {
		log.Fatal(err.Error())
	}
}
