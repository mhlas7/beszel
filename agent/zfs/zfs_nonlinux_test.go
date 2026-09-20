//go:build testing && !linux

package zfs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectorsUseUtilitiesOnNonLinux(t *testing.T) {
	oldCommandOutput := commandOutput
	commandOutput = func(name string, args ...string) ([]byte, error) {
		switch name {
		case "zpool":
			return []byte("tank\t100\t50\t50\tONLINE\n"), nil
		case "zfs":
			// Deliberately different from the zpool values so the usable
			// capacity override cannot pass unnoticed.
			return []byte("tank\t30\t50\t/tank\n"), nil
		default:
			t.Fatalf("unexpected command %s", name)
			return nil, nil
		}
	}
	t.Cleanup(func() { commandOutput = oldCommandOutput })

	pools, err := PoolStats()
	require.NoError(t, err)
	assert.Equal(t, []PoolStat{{Name: "tank", Size: 80, Alloc: 30, Free: 50, Health: "ONLINE"}}, pools)
	datasets, err := Datasets()
	require.NoError(t, err)
	assert.Equal(t, []Dataset{{Name: "tank", Used: 30, Avail: 50, Mountpoint: "/tank"}}, datasets)
	roots, err := RootDatasets()
	require.NoError(t, err)
	assert.Equal(t, []Dataset{{Name: "tank", Used: 30, Avail: 50, Mountpoint: "/tank"}}, roots)
}
