// Package port declara as portas de tempo e identidade usadas pelos casos de
// uso (decisão D7): o domínio recebe id e instante por parâmetro, e os testes
// podem fixar os dois.
package port

import (
	"time"

	"github.com/google/uuid"
)

// Clock fornece o instante atual.
type Clock interface {
	Now() time.Time
}

// IDGenerator fornece identificadores novos.
type IDGenerator interface {
	NewID() uuid.UUID
}

// SystemClock é o relógio real, em UTC.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// UUIDv7 gera UUIDs v7 (ordenados pelo tempo). uuid.Must só entraria em
// pânico se a fonte de aleatoriedade falhasse; desde o Go 1.24,
// crypto/rand.Read não devolve erro, então isso não acontece na prática.
type UUIDv7 struct{}

func (UUIDv7) NewID() uuid.UUID { return uuid.Must(uuid.NewV7()) }
