.PHONY: build host-build tui-build login host-run tui stop run proxy metrics clean clean-creds
build:
	podman build -t clawson .
host-build: build
	podman build -t clawson-host -f host.Dockerfile .
tui-build:
	podman build -t clawson-tui -f tui.Dockerfile .
login: host-build
	@mkdir -p creds
	podman run --rm -it --security-opt label=disable -v $(PWD)/creds:/root/.claude --entrypoint claude clawson-host auth login
host-run: host-build
	./run-host.sh
tui: tui-build
	@test -S $(PWD)/run/clawson.sock || { echo "no run/clawson.sock — run \`make host-run\` first"; exit 1; }
	podman run --rm -it \
	  --network=none \
	  --security-opt label=disable \
	  -v $(PWD)/run:/clawson-run \
	  -v $(PWD)/tui/src:/app/src:ro \
	  -v /etc/localtime:/etc/localtime:ro \
	  clawson-tui
stop:
	-podman rm -f cs_host
	podman ps -aq -f name=cs_ | xargs -r podman rm -f
run:
	python3 nc.py
proxy:
	python3 proxy.py
metrics:
	@jq -s 'group_by(.group)|map({group:.[0].group,n:length,usage:(map(.usage)|add)})' metrics.jsonl 2>/dev/null || tail -n 20 metrics.jsonl
clean: stop
	rm -rf groups metrics.jsonl groups.json proxy.log run
clean-creds:
	rm -rf creds
