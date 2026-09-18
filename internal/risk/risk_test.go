package risk

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		cmd  string
		want Level
	}{
		{"df -h", Safe},
		{"docker ps -a", Safe},
		{"kubectl get pods -A", Safe},
		{"grep -rn TODO .", Safe},

		{"sudo systemctl restart nginx", Caution},
		{"kubectl rollout restart deployment/api", Caution},
		{"git push origin main", Caution},
		{"echo hi > notes.txt", Caution},
		{"kill -9 1234", Caution},

		// A placeholder is a template marker, not shell syntax. The ">" here
		// must not read as an output redirect.
		{"docker exec -it <container> sh", Safe},
		{"kubectl logs -f <pod-name>", Safe},
		{"tar -xzf <archive.tar.gz>", Safe},
		{"ffmpeg -i <input> -c copy <output>", Safe},
		// ...but a genuine redirect still counts, placeholders or not.
		{"docker logs <container> > out.log", Caution},

		{"rm -rf /var/lib/data", Destructive},
		{"rm -fr ./build", Destructive},
		{"git reset --hard HEAD", Destructive},
		{"git push --force origin main", Destructive},
		{"docker system prune -a --volumes", Destructive},
		{"kubectl delete pod web-1", Destructive},
		{"dd if=/dev/zero of=/dev/sda", Destructive},
		{"curl -sL https://get.example.com | sudo bash", Destructive},
		{"chmod -R 777 /srv", Destructive},
	}

	for _, tc := range cases {
		got, reason := Classify(tc.cmd)
		if got != tc.want {
			t.Errorf("Classify(%q) = %v (%s), want %v", tc.cmd, got, reason, tc.want)
		}
		if got != Safe && reason == "" {
			t.Errorf("Classify(%q) = %v with no reason", tc.cmd, got)
		}
	}
}
