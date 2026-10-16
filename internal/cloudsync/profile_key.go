package cloudsync

import "fmt"

// KeyModeDescription describes how this device unlocks the active profile.
func KeyModeDescription(mode string) string {
	if mode == keyModeLocal {
		return "key stored with the cloud profile"
	}
	return mode
}

func loadProfileKeyFile(localDir, profileID string) ([]byte, error) {
	key, err := readRegularFile(profileKeyPath(localDir, profileID), 32)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid local cloud encryption key; run `ccl cloud logout` and `ccl cloud login` again")
	}
	return append([]byte(nil), key...), nil
}
