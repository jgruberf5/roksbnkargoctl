package cli

import (
	"fmt"
	"os"
)

// CLIImageRepo is where CI publishes roksbnkargoctl itself as a container image
// (.github/workflows/cli-image.yml): :vX.Y.Z and :latest on a release tag, :dev
// from main.
const CLIImageRepo = "ghcr.io/jgruberf5/roksbnkargoctl"

// containerEnv is set to "1" by build/cli/Dockerfile. It is how the binary knows
// it is the published image, where the binary lives read-only inside the image
// and no agent CLI is installed. A variable the image sets is used rather than
// sniffing /.dockerenv or cgroups: those also match a binary a user copied into
// their own container, where self update and agent work normally.
const containerEnv = "ROKSBNKARGOCTL_CONTAINER"

func inContainerImage() bool { return os.Getenv(containerEnv) == "1" }

// errSelfUpdateInContainer refuses self update in the image. Replacing the
// binary inside a container is lost when it exits (--rm), and the binary is
// root-owned while the image runs as non-root, so it could only half-work: a
// download followed by a permission error. tag is the release asked for with
// --version, if any.
func errSelfUpdateInContainer(tag string) error {
	if tag == "" {
		tag = "latest"
	}
	return fmt.Errorf("self update does not work in the %s image: the binary is part of the image, "+
		"so update by pulling a newer image tag instead:\n  docker pull %s:%s\n"+
		"(`self update --check` lists the releases)", CLIImageRepo, CLIImageRepo, tag)
}

// errSelfInstallInContainer refuses self install in the image: it would copy the
// binary onto a PATH inside a container that is gone when it exits.
func errSelfInstallInContainer() error {
	return fmt.Errorf("self install does not work in the %s image: it would copy the binary onto a PATH "+
		"inside this container, which is gone when it exits. Keep running the image, or install the binary "+
		"on the host from https://github.com/%s/releases", CLIImageRepo, selfRepo)
}

// errAgentInContainer refuses `agent <cli>` in the image, which carries no agent
// CLI. Without this the command would scaffold AGENTS.md into the workspace and
// then fail on a missing binary (or on stdout not being a terminal), which
// reads as something to fix inside the container.
func errAgentInContainer(cli string) error {
	return fmt.Errorf("no agent CLIs are installed in the %s image, so `agent %s` cannot start one; "+
		"print the command to run on the host with `roksbnkargoctl agent %s --show`, "+
		"or run roksbnkargoctl on the host, where %s is installed", CLIImageRepo, cli, cli, cli)
}
