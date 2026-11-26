package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// fixBuildx replaces the proxy's address, which is gone once the command exits, in the builders and
// the current builder that buildx saved, so that they keep working in later commands.
// shortcut: buildx's <dir>/.lock is not taken, so a buildx command that saves the same file at the
// same time can lose its change; take the lock if that ever shows up.
func fixBuildx(configDir, proxyHost, upstream string) {
	dir := os.Getenv("BUILDX_CONFIG")
	if dir == "" {
		dir = filepath.Join(configDir, "buildx")
	}
	endpoint := upstream
	if upstream == defaultHost {
		// The docker context, which buildx resolves at each use through the DOCKER_HOST of that command.
		endpoint = "default"
	}
	from, _ := json.Marshal(proxyHost)
	to, _ := json.Marshal(endpoint)
	files, _ := filepath.Glob(filepath.Join(dir, "instances", "*"))
	for _, file := range files {
		// The address is random, so a byte match cannot hit anything else.
		rewriteFile(file, func(data []byte) []byte { return bytes.ReplaceAll(data, from, to) })
	}
	rewriteFile(filepath.Join(dir, "current"), func(data []byte) []byte {
		var current map[string]any
		if json.Unmarshal(data, &current) != nil || current["Key"] != proxyHost {
			return data
		}
		// The key can never match again, and behind the wrapper all commands share one endpoint.
		current["Global"] = true
		data, _ = json.Marshal(current)
		return data
	})
}

// rewriteFile replaces the file with what change returns, if that differs, in one rename as
// buildx's own writes do.
func rewriteFile(path string, change func([]byte) []byte) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	changed := change(data)
	if bytes.Equal(changed, data) {
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".dond-shim-*")
	if err != nil {
		debugf("buildx: %v", err)
		return
	}
	_, err = temp.Write(changed)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(temp.Name())
		debugf("buildx: %v", err)
	}
}
