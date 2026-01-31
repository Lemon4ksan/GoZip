package gozip

// ConflictAction defines the decision taken when a file name collision occurs during [Zip.Load].
type ConflictAction int

const (
	// ActionReplace overwrites the existing entry with the new one (Default).
	ActionReplace ConflictAction = iota

	// ActionSkip ignores the new entry, keeping the existing one.
	ActionSkip

	// ActionError reports ErrDuplicateEntry and skips the new entry.
	ActionError

	// ActionRename adds the new entry under a different name provided by the resolver.
	ActionRename
)

// ConflictHandler determines how to handle name collisions.
// It receives the existing file (already in archive) and the new file (being loaded).
// It returns the action to take and an optional new name (used only with ActionRename).
type ConflictHandler func(existing, new *File) (ConflictAction, string)

// DefaultConflictHandler replaces existing files (Last Write Wins).
func DefaultConflictHandler(_, _ *File) (ConflictAction, string) {
	return ActionReplace, ""
}

// SkipConflictHandler keeps existing files and ignores new ones.
func SkipConflictHandler(_, _ *File) (ConflictAction, string) {
	return ActionSkip, ""
}

// ErrorConflictHandler treats duplicates as errors.
func ErrorConflictHandler(_, _ *File) (ConflictAction, string) {
	return ActionError, ""
}

// UpdateConflictHandler replaces the file only if the new one is newer (ModTime).
func UpdateConflictHandler(existing, new *File) (ConflictAction, string) {
	if new.ModTime().After(existing.ModTime()) {
		return ActionReplace, ""
	}
	return ActionSkip, ""
}
