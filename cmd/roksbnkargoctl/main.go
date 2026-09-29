// Command roksbnkargoctl installs F5 BIG-IP Next for Kubernetes 2.4 GA on IBM
// Cloud ROKS as an Argo CD Application.
package main

import (
	"os"

	"github.com/jgruberf5/roksbnkargoctl/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
