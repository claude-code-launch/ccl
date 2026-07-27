//go:build !darwin

package cloudsync

func storePlatformKey(string, []byte) error {
	return ErrKeychainUnavailable
}

func loadPlatformKey(string) ([]byte, error) {
	return nil, ErrKeychainUnavailable
}
