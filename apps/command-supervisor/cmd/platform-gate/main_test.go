package main

import "testing"

func TestResolveCurrentCgroupV2(t *testing.T) {
	for _, test := range []struct {
		name      string
		mountInfo string
		self      string
		wantDir   string
		wantPath  string
		wantOK    bool
	}{
		{
			name:      "namespaced cgroup root",
			mountInfo: "29 23 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n",
			self:      "0::/\n",
			wantDir:   "/sys/fs/cgroup",
			wantPath:  "/",
			wantOK:    true,
		},
		{
			name:      "host hierarchy mounted from root",
			mountInfo: "29 23 0:26 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			self:      "0::/kubepods/pod-a/container-b\n",
			wantDir:   "/sys/fs/cgroup/kubepods/pod-a/container-b",
			wantPath:  "/kubepods/pod-a/container-b",
			wantOK:    true,
		},
		{
			name:      "mount rooted at pod cgroup",
			mountInfo: "29 23 0:26 /kubepods/pod-a /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			self:      "0::/kubepods/pod-a/container-b\n",
			wantDir:   "/sys/fs/cgroup/container-b",
			wantPath:  "/kubepods/pod-a/container-b",
			wantOK:    true,
		},
		{
			name:      "legacy hierarchy",
			mountInfo: "30 23 0:27 / /sys/fs/cgroup/cpu rw - cgroup cgroup rw,cpu\n",
			self:      "2:cpu:/sandbox\n",
			wantOK:    false,
		},
		{
			name:      "unrelated cgroup2 mount root",
			mountInfo: "29 23 0:26 /other /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
			self:      "0::/kubepods/pod-a\n",
			wantPath:  "/kubepods/pod-a",
			wantOK:    false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotDir, gotPath, gotOK := resolveCurrentCgroupV2(test.mountInfo, test.self)
			if gotDir != test.wantDir || gotPath != test.wantPath || gotOK != test.wantOK {
				t.Fatalf("resolveCurrentCgroupV2() = (%q, %q, %t), want (%q, %q, %t)", gotDir, gotPath, gotOK, test.wantDir, test.wantPath, test.wantOK)
			}
		})
	}
}

func TestCountNonemptyLines(t *testing.T) {
	if got := countNonemptyLines("0::/\n1:name:/x\n\n"); got != 2 {
		t.Fatalf("countNonemptyLines() = %d, want 2", got)
	}
}
