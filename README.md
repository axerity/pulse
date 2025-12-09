# Pulse

A Pusher Protocol v7 compatible server written in Go. Pulse implements the official Pusher Channels protocol, allowing you to use any Pusher SDK with your own self-hosted server.

## Installation

### Build from Source

Requirements: Go 1.21 or higher

```bash
# Clone the repository
git clone https://github.com/aelpxy/pulse.git
cd pulse

# Build binary
go build -ldflags="-s -w" -o pulse

# Run the server
./pulse -config=config.json
```

## Usage Examples

### Server Example (TypeScript)

```typescript
import Pusher from "pusher";

const pusher = new Pusher({
  appId: "your-pulse-dev",
  key: "your-pulse-dev-key",
  secret: "your-pulse-dev-secret",
  useTLS: false,
  host: "localhost:8080",
});

setInterval(async () => {
  try {
    const timestamp = new Date().toISOString();
    const message = {
      text: "Hello from Pulse!",
      timestamp,
      count: Math.floor(Math.random() * 100),
    };

    await pusher.trigger("my-channel", "my-event", message);
  } catch (error) {}
}, 5000);
```

### Client Example (TypeScript)

```typescript
import Pusher from "pusher-js";

const pusher = new Pusher("your-pulse-dev-key", {
  wsHost: "localhost",
  wsPort: 8080,
  forceTLS: false,
  enableStats: false,
  enabledTransports: ["ws", "wss"],
});

const channel = pusher.subscribe("my-channel");

channel.bind("my-event", (data: any) => {
  console.log(data);
  console.log("");
});
```

## Configuration

An example config (name this as `config.json` then pass it via flag `-config=path`)

```json
{
  "server": {
    "debug": true,
    "port": "8080",
    "hostname": "",
    "region": ""
  },
  "apps": [
    {
      "id": "your-pulse-dev",
      "key": "your-pulse-dev-key",
      "secret": "your-pulse-dev-secret",
      "name": "pulse app",
      "enabled": true,
      "max_connections": 10000,
      "max_channels_per_connection": 50,
      "allowed_origins": ["*"],
      "max_message_size": 8192,
      "max_batch_events": 100,
      "enable_client_events": true,
      "max_event_rate": 10,
      "max_event_burst": 20
    }
  ]
}
```

### Configuration Properties

#### Server Properties

| Property | Type | Description |
|----------|------|-------------|
| `debug` | boolean | Enable debug logging for troubleshooting |
| `port` | string | Port number the server listens on |
| `hostname` | string | Hostname/IP the server binds to (empty for all interfaces) |
| `region` | string | Region identifier (used for clustering) |

#### App Properties

| Property | Type | Description |
|----------|------|-------------|
| `id` | string | Unique identifier for the application |
| `key` | string | Public key used by clients to connect |
| `secret` | string | Secret key used for authentication and signing |
| `name` | string | Human-readable name for the application |
| `enabled` | boolean | Whether the app is active and accepting connections |
| `max_connections` | number | Maximum number of concurrent WebSocket connections |
| `max_channels_per_connection` | number | Maximum channels a single connection can subscribe to |
| `allowed_origins` | array | CORS allowed origins (`["*"]` for all origins) |
| `max_message_size` | number | Maximum message size in bytes |
| `max_batch_events` | number | Maximum number of events in a batch trigger |
| `enable_client_events` | boolean | Allow clients to trigger events prefixed with `client-` |
| `max_event_rate` | number | Maximum client events per second (default: 10) |
| `max_event_burst` | number | Maximum burst capacity for client events (default: 20) |

## License

[MIT](./LICENSE)
