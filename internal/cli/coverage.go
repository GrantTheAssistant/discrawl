package cli

import (
	"errors"
	"flag"
	"io"
	"strings"

	"github.com/openclaw/discrawl/internal/discorddesktop"
	"github.com/openclaw/discrawl/internal/store"
)

type wiretapProgress struct {
	Import   discorddesktop.Stats `json:"import"`
	Coverage store.CoverageReport `json:"coverage"`
	Delta    *store.CoverageDelta `json:"delta,omitempty"`
}

func (r *runtime) runCoverage(args []string) error {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	guildID := fs.String("guild", "", "")
	jsonOut := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 0 {
		return usageErr(errors.New("coverage takes flags only"))
	}
	if *jsonOut {
		r.json = true
	}
	if r.store == nil {
		return dbErr(errors.New("coverage requires a local SQLite archive"))
	}
	report, err := r.store.Coverage(r.ctx, strings.TrimSpace(*guildID), r.nowUTC())
	if err != nil {
		return err
	}
	return r.print(report)
}
