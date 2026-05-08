.PHONY: build host-build login host-run tui stop run tui-bare proxy metrics clean clean-creds
build:
	podman build -t clawson .
host-build: build
	podman build -t clawson-host -f host.Dockerfile .
login: host-build
	@mkdir -p creds
	podman run --rm -it --security-opt label=disable -v $(PWD)/creds:/root/.claude --entrypoint claude clawson-host auth login
host-run: host-build
	./run-host.sh
tui:
	podman exec -it cs_host python3 tui.py
stop:
	-podman rm -f cs_host
	podman ps -aq -f name=cs_ | xargs -r podman rm -f
run:
	python3 nc.py
tui-bare:
	python3 tui.py
proxy:
	python3 proxy.py
metrics:
	@jq -s 'group_by(.group)|map({group:.[0].group,n:length,usage:(map(.usage)|add)})' metrics.jsonl 2>/dev/null || tail -n 20 metrics.jsonl
clean: stop
	rm -rf groups metrics.jsonl groups.json proxy.log clawson.sock
clean-creds:
	rm -rf creds
