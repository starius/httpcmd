# httpcmd

Tiny HTTP server that runs pre-configured commands in pre-configured working directories.

## Usage

```bash
cp config.example.json config.json

go run . -config config.json
```

Then hit the endpoints (GET or POST):

```bash
curl http://localhost:8080/status
```

## Config

`config.json` must include a list of endpoints with path, command, and work_dir.

```json
{
  "addr": ":8080",
  "default_timeout_seconds": 30,
  "endpoints": [
    {
      "path": "/status",
      "command": ["/bin/sh", "-c", "echo ok"],
      "work_dir": "/tmp",
      "timeout_seconds": 5,
      "pty": true
    }
  ]
}
```

- `addr`: optional; defaults to `:8080`
- `default_timeout_seconds`: optional; 0 means no timeout
- `timeout_seconds`: optional per endpoint; overrides default
- `pty`: optional; when true, runs the command in a pseudo-terminal (stdout/stderr combined)
