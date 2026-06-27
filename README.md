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
```

The default database is SQLite at `./sns_gin_gorm.db` for local verification. Set `DATABASE_URL=postgres://...` to use PostgreSQL.
