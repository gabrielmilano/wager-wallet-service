//go:build integration

// Package integration contém os testes que rodam contra o ambiente real do
// Docker Compose (ADR 0007). Pré-requisito: make up.
//
//	make test-integration
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// env lê uma variável com padrão igual ao do .env.example, para que os
// testes rodem sem .env.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// --- PostgreSQL --------------------------------------------------------------

func connect(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("conectar ao PostgreSQL: %v (o ambiente está no ar? make up)", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func TestPostgresRuntimeRoleIsRestricted(t *testing.T) {
	ctx := testContext(t)
	conn := connect(t, ctx, env("DATABASE_URL",
		"postgres://app_runtime:app_runtime@localhost:5432/wager_wallet?sslmode=disable"))

	var user string
	var super bool
	err := conn.QueryRow(ctx,
		`SELECT current_user, rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&user, &super)
	if err != nil {
		t.Fatal(err)
	}
	if user != "app_runtime" || super {
		t.Errorf("current_user = %q, superusuário = %v; want app_runtime sem superusuário", user, super)
	}

	// app_runtime não pode criar objetos: o schema é do app_migrator.
	_, err = conn.Exec(ctx, `CREATE TABLE integration_probe (id int)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("CREATE TABLE como app_runtime: erro = %v, want SQLSTATE 42501 (insufficient_privilege)", err)
	}
}

func TestPostgresMigratorOwnsSchema(t *testing.T) {
	ctx := testContext(t)
	conn := connect(t, ctx, env("MIGRATIONS_DATABASE_URL",
		"postgres://app_migrator:app_migrator@localhost:5432/wager_wallet?sslmode=disable"))

	var owner string
	err := conn.QueryRow(ctx,
		`SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = 'public'`).Scan(&owner)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "app_migrator" {
		t.Errorf("dono do schema public = %q, want app_migrator", owner)
	}

	// Cria dentro de uma transação desfeita: não deixa resíduo no banco.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TABLE integration_probe (id int)`); err != nil {
		t.Errorf("CREATE TABLE como app_migrator: %v", err)
	}
}

// --- SQS (LocalStack) --------------------------------------------------------

func sqsClient(t *testing.T, ctx context.Context) *sqs.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(env("AWS_REGION", "us-east-1")),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			env("AWS_ACCESS_KEY_ID", "test"), env("AWS_SECRET_ACCESS_KEY", "test"), "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(env("AWS_ENDPOINT_URL", "http://localhost:4566"))
	})
}

func queueAttributes(t *testing.T, ctx context.Context, c *sqs.Client, name string) map[string]string {
	t.Helper()
	u, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		t.Fatalf("fila %s não encontrada: %v", name, err)
	}
	out, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       u.QueueUrl,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Attributes
}

func TestSQSQueuesAreProvisioned(t *testing.T) {
	ctx := testContext(t)
	c := sqsClient(t, ctx)

	input := env("SQS_INPUT_QUEUE_NAME", "wager-transactions.fifo")
	dlq := env("SQS_INPUT_DLQ_NAME", "wager-transactions-dlq.fifo")
	events := env("SQS_EVENTS_QUEUE_NAME", "wallet-events.fifo")

	attrs := map[string]map[string]string{}
	for _, name := range []string{input, dlq, events} {
		a := queueAttributes(t, ctx, c, name)
		if a["FifoQueue"] != "true" {
			t.Errorf("%s: FifoQueue = %q, want true", name, a["FifoQueue"])
		}
		if a["ContentBasedDeduplication"] != "false" {
			t.Errorf("%s: ContentBasedDeduplication = %q, want false", name, a["ContentBasedDeduplication"])
		}
		attrs[name] = a
	}

	// A entrada redireciona para a DLQ depois de 5 recebimentos.
	var redrive map[string]any
	if err := json.Unmarshal([]byte(attrs[input]["RedrivePolicy"]), &redrive); err != nil {
		t.Fatalf("%s: RedrivePolicy inválida: %v", input, err)
	}
	if got, want := redrive["deadLetterTargetArn"], attrs[dlq]["QueueArn"]; got != want {
		t.Errorf("deadLetterTargetArn = %v, want %v", got, want)
	}
	if got := fmt.Sprint(redrive["maxReceiveCount"]); got != "5" {
		t.Errorf("maxReceiveCount = %s, want 5", got)
	}
	if got := attrs[input]["VisibilityTimeout"]; got != "30" {
		t.Errorf("VisibilityTimeout = %s, want 30", got)
	}
}

// TestSQSFifoRoundTrip envia e recebe numa fila FIFO temporária, para provar
// que o broker funciona sem tocar nas filas que a aplicação usa.
func TestSQSFifoRoundTrip(t *testing.T) {
	ctx := testContext(t)
	c := sqsClient(t, ctx)

	name := fmt.Sprintf("integration-probe-%d.fifo", time.Now().UnixNano())
	q, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(name),
		Attributes: map[string]string{"FifoQueue": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: q.QueueUrl})
	})

	// Duas entregas com o mesmo MessageDeduplicationId: o SQS FIFO guarda uma.
	for range 2 {
		_, err := c.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:               q.QueueUrl,
			MessageBody:            aws.String(`{"probe":true}`),
			MessageGroupId:         aws.String("wallet-1"),
			MessageDeduplicationId: aws.String("event-1"),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	out, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            q.QueueUrl,
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("recebidas %d mensagens, want 1 (deduplicação FIFO)", len(out.Messages))
	}
	if got := aws.ToString(out.Messages[0].Body); got != `{"probe":true}` {
		t.Errorf("corpo = %q", got)
	}
}

// --- Keycloak ----------------------------------------------------------------

// tokenClaims pede um token via client_credentials e devolve o status HTTP e
// os claims do payload. O payload é só decodificado, não validado: aqui o
// objetivo é conferir o provisionamento do realm. A validação de assinatura
// é da aplicação (Fase 07).
func tokenClaims(t *testing.T, ctx context.Context, clientID, secret string) (int, map[string]any) {
	t.Helper()
	issuer := env("OIDC_ISSUER_URL", "http://localhost:8180/realms/wager")
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pedir token: %v (o ambiente está no ar? make up)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(body.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("token não é um JWT: %d partes", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, claims
}

// audiences normaliza o claim aud, que pode ser string ou lista.
func audiences(claims map[string]any) []string {
	switch v := claims["aud"].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, a := range v {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func realmRoles(claims map[string]any) []string {
	ra, _ := claims["realm_access"].(map[string]any)
	raw, _ := ra["roles"].([]any)
	var out []string
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestKeycloakClientTokens(t *testing.T) {
	issuer := env("OIDC_ISSUER_URL", "http://localhost:8180/realms/wager")
	audience := env("OIDC_AUDIENCE", "wager-wallet-service")

	tests := []struct {
		clientID       string
		secretEnv      string
		wantProviderID any // nil = claim ausente
		wantRole       string
	}{
		{"provider-a", "PROVIDER_A_CLIENT_SECRET", "provider-a", "provider"},
		{"provider-b", "PROVIDER_B_CLIENT_SECRET", "provider-b", "provider"},
		{"wallet-internal", "WALLET_INTERNAL_CLIENT_SECRET", nil, "wallet-admin"},
	}
	for _, tt := range tests {
		t.Run(tt.clientID, func(t *testing.T) {
			ctx := testContext(t)
			status, claims := tokenClaims(t, ctx, tt.clientID, env(tt.secretEnv, tt.clientID+"-secret"))
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}

			if claims["iss"] != issuer {
				t.Errorf("iss = %v, want %s", claims["iss"], issuer)
			}
			if !slices.Contains(audiences(claims), audience) {
				t.Errorf("aud = %v, want conter %s", claims["aud"], audience)
			}
			if claims["provider_id"] != tt.wantProviderID {
				t.Errorf("provider_id = %v, want %v", claims["provider_id"], tt.wantProviderID)
			}
			if !slices.Contains(realmRoles(claims), tt.wantRole) {
				t.Errorf("roles = %v, want conter %s", realmRoles(claims), tt.wantRole)
			}
		})
	}
}

func TestKeycloakRejectsWrongSecret(t *testing.T) {
	ctx := testContext(t)
	status, _ := tokenClaims(t, ctx, "provider-a", "secret-errado")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

func TestKeycloakJWKSIsReachable(t *testing.T) {
	ctx := testContext(t)
	issuer := env("OIDC_ISSUER_URL", "http://localhost:8180/realms/wager")
	// Do host, o JWKS fica no próprio issuer; a app usa OIDC_JWKS_URL (rede interna).
	jwks := issuer + "/protocol/openid-connect/certs"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwks, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(set.Keys) == 0 {
		t.Errorf("JWKS: status %d com %d chaves; want 200 com ao menos uma", resp.StatusCode, len(set.Keys))
	}
}

// --- Aplicação ---------------------------------------------------------------

func TestAppLiveness(t *testing.T) {
	ctx := testContext(t)
	base := env("APP_BASE_URL", "http://localhost:8081")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /health/live: %v (o ambiente está no ar? make up)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
