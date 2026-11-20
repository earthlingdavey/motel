package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// rulesNow is the command's clock, for how old a reading is.
var rulesNow = time.Now

// rulesLimit is how much of the route's answer the command reads.
const rulesLimit = 1 << 20

func rulesCommand() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "rules [--json]",
		Short: "List every alert motel can raise here, and what each fires on (root)",
		Long: `Asks the running moteld over its admin socket (root and the motel user
reach it) for every alert it can raise on this config: its source
(builtin, operator or motel check), severity, scope and kind
(threshold, guarded or event).

A threshold the metrics store evaluates comes with its op and
threshold, its for, its guards, and its numbers: moteld runs its value's
MetricsQL (value_query) on the store's socket itself, at most every 30 s
for this command and every person's read of the rules together, and
lists for each app or filesystem the value, and whether vmalert holds it
pending or firing and since when (--json: readings, and store.values,
unanswered when the store did not answer). A check motel makes itself
(disk_low, backup_stale, watchdog_throttled, metrics_store_down,
metrics_rules_down) comes with the number it last read. With no metrics
line in the monitor block, the store's rules are not listed. When rules.yml, the file
vmalert reads, is not this config's (a write at a load that failed),
the table ends with a line that says so, and --json has rules_file.
docs/monitor.md, "How close is each alert", has an example.

    moteld monitor rules
    moteld monitor rules --json`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return printAnswer(c, asJSON, func() (RulesListing, error) {
				out, err := adminRequest(c, http.MethodGet, "/notify/rules", nil, rulesLimit)
				if err != nil {
					return RulesListing{}, notAdmitted(c, err, "moteld monitor rules")
				}
				var l RulesListing
				if json.Unmarshal(out, &l) != nil || l.Alerts == nil {
					if len(out) >= rulesLimit {
						return RulesListing{}, fmt.Errorf("motel's listing is over %d bytes, more than this command reads", rulesLimit)
					}
					return RulesListing{}, fmt.Errorf("motel answered %q", strings.TrimSpace(string(out)))
				}
				return l, nil
			}, func(w io.Writer, l RulesListing) error {
				_, err := w.Write(rulesText(l, rulesNow()))
				return err
			})
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, `print the listing as one JSON object, or {"error":…}`)
	adminFlags(c)
	return c
}
