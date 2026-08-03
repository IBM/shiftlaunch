// Package utils provides small, reusable helpers shared across shiftlaunch packages.
package utils

import (
	"regexp"
	"strings"
)

// versionRe matches a valid OpenShift version: MAJOR.MINOR.PATCH with an optional pre-release suffix.
// Examples: 4.17.3, 4.21.0-rc.2, 4.21.0-0.nightly-2025-01-01
var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-\S+)?$`)

// Contains reports whether item is present in slice.
func Contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// IsValidVersion reports whether version matches the expected MAJOR.MINOR.PATCH[‑suffix] format.
func IsValidVersion(version string) bool {
	return versionRe.MatchString(version)
}

// IsPreReleaseVersion returns true if the version string contains any pre-release
// marker that would not have a stable path on mirror.openshift.com.
// Examples: 4.21.0-ec.1, 4.21.0-rc.2, 4.21.0-candidate, 4.21.0-0.nightly-2025-01-01
func IsPreReleaseVersion(version string) bool {
	lower := strings.ToLower(version)
	for _, marker := range []string{"ec", "rc", "candidate", "nightly", "pre", "alpha", "beta"} {
		if strings.Contains(lower, "-"+marker) {
			return true
		}
	}
	return false
}
