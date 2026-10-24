package utils

import (
	"fmt"
	"strings"
)

// ValidateSkillIdentifier validates that the given skill identifier (slug or registry name) is non-empty
// and does not contain path separators ("/", "\\") or ".." for security.
func ValidateSkillIdentifier(identifier string) error {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return fmt.Errorf("identifier is required and must be a non-empty string")
	}
	if strings.ContainsAny(trimmed, "/\\") || strings.Contains(trimmed, "..") {
		return fmt.Errorf("identifier must not contain path separators or '..' to prevent directory traversal")
	}
	return nil
}

// ParseOwnerSkillRef splits an owner-qualified skill reference ("@owner/slug"
// or "owner/slug") into its owner handle and slug. A bare slug returns an empty
// owner. Each segment is restricted to [A-Za-z0-9._-], must start with an
// alphanumeric, and may not contain "..", so neither segment can be used for
// path traversal. ValidateSkillIdentifier is intentionally left strict because
// it also validates registry names and local skill names.
func ParseOwnerSkillRef(ref string) (owner, slug string, err error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return "", "", fmt.Errorf("identifier is required and must be a non-empty string")
	}
	hadAt := strings.HasPrefix(trimmed, "@")
	trimmed = strings.TrimPrefix(trimmed, "@")
	parts := strings.Split(trimmed, "/")
	switch {
	case len(parts) == 1 && !hadAt:
		slug = parts[0]
	case len(parts) == 2:
		owner, slug = parts[0], parts[1]
		if !validSkillRefSegment(owner) {
			return "", "", fmt.Errorf("invalid owner %q in %q", owner, ref)
		}
	default:
		return "", "", fmt.Errorf("identifier must be 'slug', 'owner/slug' or '@owner/slug'")
	}
	if !validSkillRefSegment(slug) {
		return "", "", fmt.Errorf("invalid slug %q in %q", slug, ref)
	}
	return owner, slug, nil
}

func validSkillRefSegment(s string) bool {
	if s == "" || strings.Contains(s, "..") {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '-' || r == '_' || r == '.'):
		default:
			return false
		}
	}
	return true
}
