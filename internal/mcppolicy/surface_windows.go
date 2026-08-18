//go:build windows

package mcppolicy

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

type registrySurfaces struct {
	system bool
	user   bool
}

func detectClaudeRegistrySurfaces() (registrySurfaces, error) {
	system, systemErr := registrySettingsPresent(registry.LOCAL_MACHINE)
	user, userErr := registrySettingsPresent(registry.CURRENT_USER)
	if systemErr != nil || userErr != nil {
		return registrySurfaces{system: system, user: user}, errors.New("Claude managed registry policy cannot be inspected")
	}
	return registrySurfaces{system: system, user: user}, nil
}

func registrySettingsPresent(root registry.Key) (bool, error) {
	key, err := registry.OpenKey(root, `SOFTWARE\Policies\ClaudeCode`, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return true, err
	}
	defer key.Close()
	_, _, err = key.GetStringValue("Settings")
	if err == nil {
		return true, nil
	}
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	return true, err
}
