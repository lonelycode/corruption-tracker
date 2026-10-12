// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package settings

// SettingKind reports whether name is a setting this version knows, and
// whether it is a secret (bundle.SettingsTarget: the importer decides how
// each bundled setting is stored). Local settings (naming.enabled) report
// as unknown, so an import skips them and lists them as skipped: the
// receiving admin turns naming on themselves.
func (s *Service) SettingKind(name string) (known, secret bool) {
	k, ok := KeyByName(name)
	if !ok || k.Local {
		return false, false
	}
	return true, k.Secret
}

// SealSecret encrypts a secret setting under this instance's key, as
// stored in tracker_settings.ciphertext (bundle.SettingsTarget: the
// importer re-seals a bundle's secrets for the target instance). It is the
// inverse of OpenSecret.
func (s *Service) SealSecret(name, plaintext string) ([]byte, error) {
	box, err := s.box()
	if err != nil {
		return nil, err
	}
	return box.seal(name, plaintext)
}
