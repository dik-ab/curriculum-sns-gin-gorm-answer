# SNS API Gin + GORM Answer

Go / Gin + GORM version of the SNS curriculum answer backend.

```bash
go test ./...
go run .
```

React frontend:

```bash
VITE_API_URL="http://localhost:8000"
VITE_SOCKET_URL="http://localhost:8000"
VITE_REALTIME_DRIVER="websocket"
```

Realtime DM is verified through the plain WebSocket endpoint at `/chat`.
Some Socket.IO compatibility code and dependencies remain in this answer repo for comparison, but the shared React frontend should use `VITE_REALTIME_DRIVER="websocket"` for the Gin + GORM curriculum.

The default database is SQLite at `./sns_gin_gorm.db` for local verification. Set `DATABASE_URL=postgres://...` to use PostgreSQL.
