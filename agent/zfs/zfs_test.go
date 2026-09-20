//go:build testing

package zfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixturePath(name string) string {
	return filepath.Join("..", "test-data", "zfs", name)
}

func TestParseZpoolListOutput(t *testing.T) {
	data, err := os.ReadFile(fixturePath("zpool_list.txt"))
	require.NoError(t, err)

	pools, err := parseZpoolListOutput(data)
	require.NoError(t, err)
	require.Len(t, pools, 2)

	assert.Equal(t, PoolStat{Name: "tank", Size: 23999000000000, Alloc: 12000000000000, Free: 11999000000000, Health: "ONLINE"}, pools[0])
	assert.Equal(t, PoolStat{Name: "rpool", Size: 1200000000000, Alloc: 900000000000, Free: 300000000000, Health: "DEGRADED"}, pools[1])
}

func TestParseZpoolListOutputIgnoresEmptyLines(t *testing.T) {
	pools, err := parseZpoolListOutput([]byte("tank\t100\t50\t50\tONLINE\n\n"))
	require.NoError(t, err)
	require.Len(t, pools, 1)
	assert.Equal(t, "tank", pools[0].Name)
}

func TestParseZpoolListOutputNoPools(t *testing.T) {
	pools, err := parseZpoolListOutput([]byte("no pools available\n"))
	require.NoError(t, err)
	assert.Empty(t, pools)
}

func TestParseZpoolListOutputRejectsMalformedLine(t *testing.T) {
	_, err := parseZpoolListOutput([]byte("tank\t100\t50\n"))
	require.Error(t, err)

	_, err = parseZpoolListOutput([]byte("tank\tnotanumber\t50\t50\tONLINE\n"))
	require.Error(t, err)
}

func TestParseZfsListOutput(t *testing.T) {
	data, err := os.ReadFile(fixturePath("zfs_list.txt"))
	require.NoError(t, err)

	datasets, err := parseZfsListOutput(data)
	require.NoError(t, err)
	require.Len(t, datasets, 9)

	// Mountpoint with a space must be kept intact (tab-split only).
	assert.Equal(t, "/tank/my media", datasets[3].Mountpoint)
	// Unmounted datasets/zvols report "-".
	assert.Equal(t, "-", datasets[4].Mountpoint)
	assert.Equal(t, uint64(12000000000000), datasets[0].Used)
	assert.Equal(t, uint64(11999000000000), datasets[0].Avail)
}

// loadCapacityFixtures returns the raw `zpool list` pools and the depth-0
// root datasets. The fixtures deliberately disagree: tank is a 24 TB RAIDZ1
// whose usable capacity is ~18 TB, so a test cannot pass by accident if the
// usable values are never applied.
func loadCapacityFixtures(t *testing.T) ([]PoolStat, []Dataset) {
	t.Helper()
	poolData, err := os.ReadFile(fixturePath("zpool_list.txt"))
	require.NoError(t, err)
	pools, err := parseZpoolListOutput(poolData)
	require.NoError(t, err)

	rootData, err := os.ReadFile(fixturePath("zfs_list_depth0.txt"))
	require.NoError(t, err)
	roots, err := parseZfsListOutput(rootData)
	require.NoError(t, err)
	return pools, roots
}

func TestApplyUsableCapacity(t *testing.T) {
	pools, roots := loadCapacityFixtures(t)
	require.Len(t, pools, 2)
	// Guard the fixtures themselves: raw and usable must differ.
	require.NotEqual(t, pools[0].Size, roots[0].Used+roots[0].Avail)

	applyUsableCapacity(pools, roots)

	// tank: 24 TB of raw RAIDZ1 vdevs, ~18 TB usable.
	assert.Equal(t, uint64(17999000000000), pools[0].Size)
	assert.Equal(t, uint64(12000000000000), pools[0].Alloc)
	assert.Equal(t, uint64(5999000000000), pools[0].Free)
	assert.False(t, pools[0].Raw)

	// rpool's root dataset has mountpoint "-" and must still be matched, by
	// name rather than mountpoint.
	assert.Equal(t, uint64(1150000000000), pools[1].Size)
	assert.Equal(t, uint64(900000000000), pools[1].Alloc)
	assert.Equal(t, uint64(250000000000), pools[1].Free)
	assert.False(t, pools[1].Raw)
	// Health is still the zpool list value.
	assert.Equal(t, "DEGRADED", pools[1].Health)
}

func TestApplyUsableCapacityFallsBackToRaw(t *testing.T) {
	pools, _ := loadCapacityFixtures(t)

	applyUsableCapacity(pools, nil)

	// zpool list values are kept verbatim and flagged as physical.
	assert.Equal(t, uint64(23999000000000), pools[0].Size)
	assert.Equal(t, uint64(12000000000000), pools[0].Alloc)
	assert.Equal(t, uint64(11999000000000), pools[0].Free)
	assert.True(t, pools[0].Raw)
	assert.True(t, pools[1].Raw)
}

func TestApplyUsableCapacityIsPerPool(t *testing.T) {
	pools, roots := loadCapacityFixtures(t)

	// Drop rpool's root dataset; tank must still get usable values.
	applyUsableCapacity(pools, roots[:1])

	assert.Equal(t, uint64(17999000000000), pools[0].Size)
	assert.False(t, pools[0].Raw)
	assert.Equal(t, uint64(1200000000000), pools[1].Size)
	assert.True(t, pools[1].Raw)
}

func TestApplyUsableCapacityIgnoresChildDatasets(t *testing.T) {
	pools := []PoolStat{{Name: "tank", Size: 100, Alloc: 60, Free: 40}}

	applyUsableCapacity(pools, []Dataset{
		{Name: "tank/apps", Used: 1, Avail: 2, Mountpoint: "/tank/apps"},
	})

	assert.Equal(t, uint64(100), pools[0].Size)
	assert.True(t, pools[0].Raw)
}

func TestApplyUsableCapacityIgnoresZeroCapacityRoot(t *testing.T) {
	pools := []PoolStat{{Name: "tank", Size: 100, Alloc: 60, Free: 40}}

	applyUsableCapacity(pools, []Dataset{{Name: "tank", Used: 0, Avail: 0, Mountpoint: "/tank"}})

	assert.Equal(t, uint64(100), pools[0].Size)
	assert.True(t, pools[0].Raw)
}

func TestParseZpoolStatusOutput(t *testing.T) {
	data, err := os.ReadFile(fixturePath("zpool_status.txt"))
	require.NoError(t, err)

	pools, err := parseZpoolStatusOutput(data)
	require.NoError(t, err)
	require.Len(t, pools, 2)

	tank := pools[0]
	assert.Equal(t, "tank", tank.Name)
	assert.Equal(t, "ONLINE", tank.State)
	assert.Equal(t, "FINISHED", tank.Scrub.State)
	assert.Equal(t, "", tank.Scrub.Progress)
	assert.Equal(t, uint64(0), tank.Scrub.Errors)
	// Pool row itself is skipped; mirror + 2 disks remain.
	require.Len(t, tank.Vdevs, 3)
	assert.Equal(t, "mirror-0", tank.Vdevs[0].Name)
	assert.Equal(t, "sda", tank.Vdevs[1].Name)
	assert.Equal(t, "sdb", tank.Vdevs[2].Name)

	rpool := pools[1]
	assert.Equal(t, "rpool", rpool.Name)
	assert.Equal(t, "DEGRADED", rpool.State)
	assert.Equal(t, "SCANNING", rpool.Scrub.State)
	assert.Equal(t, "10.00%", rpool.Scrub.Progress)
	require.Len(t, rpool.Vdevs, 3)
	assert.Equal(t, "FAULTED", rpool.Vdevs[2].State)
	assert.Equal(t, uint64(1), rpool.Vdevs[2].ReadErrs)
	assert.Equal(t, uint64(2), rpool.Vdevs[2].WriteErrs)
	assert.Equal(t, uint64(3), rpool.Vdevs[2].ChecksumErrs)
}

func TestParseScanLine(t *testing.T) {
	assert.Equal(t, "FINISHED", parseScanLine("scan: scrub repaired 0B in 00:05:12 with 0 errors on Sun Jun  1 02:00:12 2025").State)
	assert.Equal(t, uint64(3), parseScanLine("scan: scrub repaired 10G in 01:00:00 with 3 errors on Sun Jun  1 02:00:12 2025").Errors)
	assert.Equal(t, "SCANNING", parseScanLine("scan: scrub in progress since Sun Jun  8 01:00:00 2025").State)
	assert.Equal(t, "CANCELED", parseScanLine("scan: scrub canceled on Sun Jun  1 02:00:12 2025").State)
	assert.Equal(t, "FINISHED", parseScanLine("scan: resilvered 1.23G in 00:01:00 with 0 errors on Sun Jun  1 02:00:12 2025").State)
	assert.Equal(t, "NONE", parseScanLine("scan: none requested").State)
}

func TestCommandOutputForcesLocaleAndTimesOut(t *testing.T) {
	t.Setenv("BESZEL_ZFS_COMMAND_HELPER", "1")
	out, err := commandOutput(os.Args[0], "-test.run=TestZfsCommandHelperProcess", "--", "locale")
	require.NoError(t, err)
	assert.Equal(t, "C/C", string(out))

	oldTimeout := commandTimeout
	commandTimeout = 20 * time.Millisecond
	t.Cleanup(func() { commandTimeout = oldTimeout })
	_, err = commandOutput(os.Args[0], "-test.run=TestZfsCommandHelperProcess", "--", "sleep")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestZfsCommandHelperProcess(t *testing.T) {
	if os.Getenv("BESZEL_ZFS_COMMAND_HELPER") != "1" {
		return
	}
	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	switch strings.TrimSpace(mode) {
	case "locale":
		_, _ = fmt.Printf("%s/%s", os.Getenv("LC_ALL"), os.Getenv("LANG"))
	case "sleep":
		time.Sleep(time.Second)
	}
	os.Exit(0)
}
