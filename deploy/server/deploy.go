package main

import (
	"github.com/alinz/script.go"

	"farsi.ella.to/deploy"
)

const remotePath = "/home/bot/farsi.ella.to/server"

func Runner(workspace string) error {
	runner, err := script.NewRunner(deploy.Config)
	if err != nil {
		return err
	}
	defer runner.Close()

	err = runner.RunLocal(
		workspace,

		// --platform is required: the deploy target is linux/amd64, and without
		// it a build from an Apple Silicon laptop produces a linux/arm64 image
		// that `docker compose up` on the server refuses to run.
		"docker build --platform linux/amd64 --no-cache --progress=plain -t farsi.ella.to:latest -f ${workspace}/deploy/server/Dockerfile ${workspace}",
		"docker save farsi.ella.to:latest | gzip > ${workspace}/deploy/server/docker.tar.gz",
	)
	if err != nil {
		return err
	}

	err = runner.CopyFiles(
		"0644",
		remotePath,
		workspace,

		"deploy/server/docker-compose.yml",
		"deploy/server/docker.tar.gz",
	)
	if err != nil {
		return err
	}

	err = runner.RunRemote(
		"docker compose -f "+remotePath+"/docker-compose.yml down",
		"! docker rmi farsi.ella.to:latest",
		"docker load -i "+remotePath+"/docker.tar.gz",
		"rm -f "+remotePath+"/docker.tar.gz",
		// The container runs as 1001:1001 and writes the sync data here; create
		// it up front so docker doesn't create it root-owned on first start.
		"mkdir -p "+remotePath+"/data",
		// NOTE: to start over with an empty catalog, uncomment the line below.
		// Be cautious: the next sync re-fetches everything (about 20 minutes).
		// "rm -rf "+remotePath+"/data/*",
		"docker compose -f "+remotePath+"/docker-compose.yml up -d",
	)
	if err != nil {
		return err
	}

	return nil
}
