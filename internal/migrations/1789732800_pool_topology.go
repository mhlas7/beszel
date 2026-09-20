package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		c, err := app.FindCollectionByNameOrId("zfs_pools")
		if err != nil {
			return err
		}
		c.Fields.Add(&core.TextField{Name: "topology"})
		return app.Save(c)
	}, func(app core.App) error {
		c, err := app.FindCollectionByNameOrId("zfs_pools")
		if err != nil {
			return err
		}
		c.Fields.RemoveByName("topology")

		return app.Save(c)
	})
}
