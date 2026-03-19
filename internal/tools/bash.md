Use when the user needs Linux shell execution, package installation, file inspection, or ad hoc command-line tooling inside an isolated Debian container.
Required param: `command`.
Optional params: `working_dir`, `timeout_ms`.
This tool runs inside a persistent container workspace, so installed packages and created files remain available until the container is recreated.
