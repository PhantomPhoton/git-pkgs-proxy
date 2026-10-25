package memlimit

import (
	"testing"
	"testing/fstest"
)

func TestCgroupLimit(t *testing.T) {
	const gib = 1 << 30
	for _, tt := range []struct {
		name  string
		files map[string]string
		want  int64
	}{
		{
			name:  "cgroup v2 container",
			files: map[string]string{"proc/self/cgroup": "0::/\n", "sys/fs/cgroup/memory.max": "2147483648\n"},
			want:  2 * gib,
		},
		{
			name: "cgroup v2 own cgroup",
			files: map[string]string{
				"proc/self/cgroup": "0::/system.slice/proxy.service\n",
				"sys/fs/cgroup/system.slice/proxy.service/memory.max": "1073741824\n",
			},
			want: gib,
		},
		{
			name:  "cgroup v2 unlimited",
			files: map[string]string{"proc/self/cgroup": "0::/\n", "sys/fs/cgroup/memory.max": "max\n"},
		},
		{
			name: "cgroup v1",
			files: map[string]string{
				"proc/self/cgroup":                           "12:cpu,cpuacct:/\n4:memory:/\n",
				"sys/fs/cgroup/memory/memory.limit_in_bytes": "536870912\n",
			},
			want: gib / 2,
		},
		{
			name: "cgroup v1 unlimited",
			files: map[string]string{
				"proc/self/cgroup":                           "4:memory:/\n",
				"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n",
			},
		},
		{
			name:  "malformed",
			files: map[string]string{"sys/fs/cgroup/memory.max": "lots\n"},
		},
		{
			name: "no cgroup files",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for name, data := range tt.files {
				fsys[name] = &fstest.MapFile{Data: []byte(data)}
			}
			got, ok := cgroupLimit(fsys)
			if got != tt.want || ok != (tt.want != 0) {
				t.Errorf("cgroupLimit = %d, %v; want %d", got, ok, tt.want)
			}
		})
	}
}
