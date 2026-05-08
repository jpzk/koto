.PHONY: build run proxy metrics host-build host-run login clean
build:
	podman build -t clawson .
host-build: build
	podman build -t clawson-host -f host.Dockerfile .
login: host-build
	@mkdir -p creds
	podman run --rm -it --security-opt label=disable -v $(PWD)/creds:/root/.claude --entrypoint claude clawson-host auth login
host-run: host-build
	./run-host.sh
run:
	python3 nc.py
proxy:
	python3 proxy.py
metrics:
	@jq -s 'group_by(.group)|map({group:.[0].group,n:length,usage:(map(.usage)|add)})' metrics.jsonl 2>/dev/null || tail -n 20 metrics.jsonl
clean:
	podman ps -aq -f name=cs_ | xargs -r podman rm -f
	rm -rf groups metrics.jsonl groups.json
clean-creds:
	rm -rf creds
