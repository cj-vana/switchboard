//go:build !windows

package mcppolicy

type registrySurfaces struct {
	system bool
	user   bool
}

func detectClaudeRegistrySurfaces() (registrySurfaces, error) {
	return registrySurfaces{}, nil
}
