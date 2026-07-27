package text

// SepDot separates segments of user-visible activity and notification text
// ("Backrest · daily-backup · local-repo"). Use it when joining; inside a
// string or format literal write the middot itself, never its \u escape. The
// escape is what made this worth a constant: it hid the separator from grep.
const SepDot = " · "
