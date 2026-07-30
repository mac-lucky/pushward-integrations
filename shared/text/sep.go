package text

// SepDot separates segments of user-visible activity and notification text
// ("Backrest · daily-backup · local-repo"). Use it when joining; inside a
// string or format literal write the middot itself, never its \u escape. The
// escape is what made this worth a constant: it hid the separator from grep.
const SepDot = " · "

// SepArrow joins the two sides of a transition in user-visible text
// ("1.2.3 → 1.2.4"). Same rule as SepDot, and the same reason: the repo-wide
// non-ASCII sweep converts every arrow it finds, so the handful that are
// deliberate product typography have to be findable as one name rather than
// scattered literals a sweep cannot tell apart from prose.
const SepArrow = " → "
