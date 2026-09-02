package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/config"
	"github.com/elk-work/ark/internal/records"
)

// Repository-level settings a person flips on purpose. Each key names one
// field of .ark/config.toml; adding a key means adding a row here and a field
// there, nothing else.
const keyRequireElkParent = "require-elk-parent"

var configKeys = []string{keyRequireElkParent}

type configValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and change this repository's Ark settings",
		Long: `Repository-level settings, kept in .ark/config.toml.

Keys:
  require-elk-parent   true|false. When true, ` + "`ark task create`" + ` and
                       ` + "`ark gh issue create`" + ` refuse a task that names no Elk
                       parent (--elk). Off by default; switch it on once every
                       open task in this repository has one.`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "get <key>",
			Short: "Print one setting",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := g.open(cmd)
				if err != nil {
					return err
				}
				defer a.Close()
				v, err := readConfigKey(a.Config, args[0])
				if err != nil {
					return err
				}
				p := g.printer(cmd)
				return p.Result(v, func() { p.Line("%s = %s", v.Key, v.Value) })
			},
		},
		&cobra.Command{
			Use:   "set <key> <value>",
			Short: "Change one setting",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := g.open(cmd)
				if err != nil {
					return err
				}
				defer a.Close()
				if err := writeConfigKey(a.Config, args[0], args[1]); err != nil {
					return err
				}
				if err := config.Save(a.ArkDir, a.Config); err != nil {
					return err
				}
				v, _ := readConfigKey(a.Config, args[0])
				p := g.printer(cmd)
				return p.Result(v, func() { p.Line("%s = %s", v.Key, v.Value) })
			},
		},
	)
	return cmd
}

func readConfigKey(c *config.Config, key string) (configValue, error) {
	switch key {
	case keyRequireElkParent:
		return configValue{Key: key, Value: strconv.FormatBool(c.RequireElkParent)}, nil
	}
	return configValue{}, records.Validationf("unknown setting %q (known: %v)", key, configKeys)
}

func writeConfigKey(c *config.Config, key, raw string) error {
	switch key {
	case keyRequireElkParent:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return records.Validationf("%s takes true or false, not %q", key, raw)
		}
		c.RequireElkParent = b
		return nil
	}
	return records.Validationf("unknown setting %q (known: %v)", key, configKeys)
}
