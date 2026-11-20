package wire

import "time"

// The rules a box alerts on, and how close each is to firing
// (notify/DESIGN-metrics.md, "How close each rule is"). RulesListing
// is `moteld monitor rules --json`, root's on the box; Rules is the
// people's, GET /_motel/rules (every rule, for a viewer of the box) and
// GET /_motel/apps/<app>/rules (the rules that apply to one app, with
// that app's numbers alone). Both carry the same AlertListing, so the
// command and the client read one shape; the people's leaves out what
// only the box can use: the store's socket and every query.

// RulesListing is `moteld monitor rules --json`: every alert motel can
// raise on the loaded config.
type RulesListing struct {
	// Store is how to read the store-backed rules; absent with no
	// metrics line, and so are those rules.
	Store *StoreQueries `json:"store,omitempty"`
	// RulesFile is set when rules.yml, the file vmalert reads, is not
	// what this config renders into it: RulesFileDiffers, RulesFileMissing
	// or RulesFileUnreadable. It speaks for the file, not for what
	// vmalert loaded.
	RulesFile string         `json:"rules_file,omitempty"`
	Alerts    []AlertListing `json:"alerts"`
}

// The words of RulesListing.RulesFile and Rules.RulesFile.
const (
	RulesFileDiffers    = "differs"
	RulesFileMissing    = "missing"
	RulesFileUnreadable = "unreadable"
)

// StoreQueries are the store's socket and how vmalert's evaluations
// line up with a query a client makes there.
type StoreQueries struct {
	Socket string `json:"socket"`
	// QueryOffsetSeconds is how far behind its run vmalert stamps an
	// evaluation, at most: its interval and its -rule.evalDelay. A value
	// query at now minus it reads no later than what vmalert compared.
	QueryOffsetSeconds int `json:"query_offset_seconds"`
	// StateQuery and ActiveSinceQuery are pending or firing, and since
	// when: vmalert writes both series for an alert that is not
	// inactive, with a gap at each evaluation that the lookback covers.
	StateQuery       string `json:"state_query"`
	ActiveSinceQuery string `json:"active_since_query"`
	// Values is StoreOn when the store answered the box's own
	// evaluation of these queries, whose numbers are each store rule's
	// Readings, or StoreUnanswered when it did not within the box's
	// budget: the store rules then have no readings, and say nothing.
	Values string `json:"values"`
}

// Rules is the people's listing: GET /_motel/rules and GET
// /_motel/apps/<app>/rules. Experimental, as every route under
// /_motel/: its shape may change until a release says otherwise.
type Rules struct {
	// Store is StoreOn (the store answered: each store rule's Readings
	// are its numbers, maybe none), StoreOff (the config has no metrics
	// line: no store rule is listed, or no monitor block that raises
	// anything, and Alerts is empty) or StoreUnanswered (the store did
	// not answer within the box's budget: the store rules have no
	// readings, and say nothing). A client reads a word it does not know
	// as not knowing any store rule's value.
	Store string `json:"store"`
	// RulesFile is as RulesListing's: on the box's read alone, never an
	// app's (the file is the box's).
	RulesFile string `json:"rules_file,omitempty"`
	// Alerts are the alerts motel can raise, in the listing's order: an
	// app's read holds those whose subject is an app, every app's or that
	// app's alone, each with that app's readings and no other's.
	Alerts []AlertListing `json:"alerts"`
}

// The words of Rules.Store and StoreQueries.Values.
const (
	StoreOn         = "on"
	StoreOff        = "off"
	StoreUnanswered = "unanswered"
)

// AlertListing is one alert name, and what it fires on.
type AlertListing struct {
	Name string `json:"name"`
	// Source is builtin (a store rule motel ships), operator (a rule
	// line) or motel check (motel's own detectors).
	Source string `json:"source"`
	// Severity is the one it is first raised at.
	Severity string `json:"severity"`
	// Subject is box or app; App the one app a rule line is pinned to.
	Subject string `json:"subject"`
	App     string `json:"app,omitempty"`
	// Kind is threshold, guarded (a threshold with guards) or event
	// (nothing to be close to).
	Kind string `json:"kind"`

	// A store rule's: ValueQuery Op Threshold is its condition, held
	// for ForSeconds. The people's listing has no ValueQuery.
	ValueQuery string         `json:"value_query,omitempty"`
	Op         string         `json:"op,omitempty"`
	Threshold  *float64       `json:"threshold,omitempty"`
	Unit       string         `json:"unit,omitempty"`
	ForSeconds int            `json:"for_seconds,omitempty"`
	Guards     []GuardListing `json:"guards,omitempty"`

	// A check's: disk_low's two levels. Readings are the numbers last
	// read: a check's, as its detector last read them; a store rule's,
	// one per series of its value (an app, a filesystem, or one for a
	// value with no label), as the box evaluated them.
	Levels   []LevelListing `json:"levels,omitempty"`
	Readings []Reading      `json:"readings,omitempty"`
}

// GuardListing is a term the rule's condition also needs: Query Op
// Value, or with no Op, Query having a series at all. The people's
// listing has no Query and no Match: its Meaning says it.
type GuardListing struct {
	Query string   `json:"query,omitempty"`
	Op    string   `json:"op,omitempty"`
	Value *float64 `json:"value,omitempty"`
	// Match is how the guard's series are matched to the value's, such
	// as "on (app)", or "on ()" for a guard with no labels, which holds
	// for every series of the value; none for all their labels.
	Match   string `json:"match,omitempty"`
	Meaning string `json:"meaning"`
}

// LevelListing is one of disk_low's: fired at Threshold, cleared under
// ClearsBelow.
type LevelListing struct {
	Severity    string  `json:"severity"`
	Threshold   float64 `json:"threshold"`
	ClearsBelow float64 `json:"clears_below"`
}

// Reading is a number at MeasuredAt: for one filesystem or one backup
// (Instance, the alert's instance), or one app (App), or for the one
// series a value with no label has (neither).
type Reading struct {
	Instance string `json:"instance,omitempty"`
	App      string `json:"app,omitempty"`
	// Value is absent only on a store rule's reading of a series vmalert
	// holds pending or firing whose value the store gave none of at
	// MeasuredAt.
	Value *float64 `json:"value,omitempty"`
	// Threshold and WindowSeconds are the watchdog's, which differ by app.
	Threshold     *float64 `json:"threshold,omitempty"`
	WindowSeconds float64  `json:"window_seconds,omitempty"`
	// Level is disk_low's severity firing now, which its gap decides.
	Level      string    `json:"level,omitempty"`
	MeasuredAt time.Time `json:"measured_at"`
	// State is a store rule's, as vmalert last wrote it to the store:
	// RuleInactive, RulePending (its condition holds, not yet for its
	// for) or RuleFiring; ActiveSince is since when it is pending or
	// firing. A client reads a word it does not know as not knowing.
	State       string     `json:"state,omitempty"`
	ActiveSince *time.Time `json:"active_since,omitempty"`
}

// The words of Reading.State.
const (
	RuleInactive = "inactive"
	RulePending  = "pending"
	RuleFiring   = "firing"
)
