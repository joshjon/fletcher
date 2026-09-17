package image

import "regexp"

var templateName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidName reports whether a template name is a safe, bounded basename.
func ValidName(name string) bool { return templateName.MatchString(name) }
