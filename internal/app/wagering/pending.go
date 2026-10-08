package wagering

import (
	"context"
	"errors"
	"time"

	"github.com/gabrielmilano/wager-wallet-service/internal/app/apperr"
	"github.com/gabrielmilano/wager-wallet-service/internal/app/store"
	"github.com/gabrielmilano/wager-wallet-service/internal/domain/wager"
)

// PermanentFailure é o código gravado quando uma pendência encontra erro
// permanente ao ser retomada (estado FAILED, decisão 3).
const PermanentFailure wager.FailureCode = "PERMANENT_FAILURE"

// RetryDelay é a espera antes da tentativa n (1-based) de resolver uma
// referência: 1 s, 2 s, 4 s... até MaxRetryDelay.
func RetryDelay(attempt int) time.Duration {
	d := FirstRetryDelay
	for i := 1; i < attempt && d < MaxRetryDelay; i++ {
		d *= 2
	}
	return min(d, MaxRetryDelay)
}

// ResumeResult descreve a retomada de uma pendência.
type ResumeResult struct {
	Transaction *wager.Transaction // nil se não havia pendência vencida
}

// ResumeNextPending retoma uma operação em PENDING_REFERENCE com tentativa
// vencida, numa transação própria. A linha é travada com FOR UPDATE SKIP
// LOCKED (outra instância pega outra pendência) e a carteira em seguida,
// mantendo a ordem operação → carteira.
//
//   - referência resolvida: aplica a movimentação ou rejeita, como no fluxo
//     síncrono, com os mesmos eventos;
//   - ainda sem referência e prazo vencido: REJECTED REFERENCE_NOT_FOUND;
//   - ainda sem referência: agenda nova tentativa com backoff.
func (s *Service) ResumeNextPending(ctx context.Context) (ResumeResult, error) {
	var res ResumeResult
	err := s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		now := s.clock.Now()
		due, err := r.Transactions.LockDuePendingReference(ctx, now, 1)
		if err != nil || len(due) == 0 {
			return err
		}
		tx := due[0]
		res.Transaction = tx

		w, err := r.Wallets.GetForUpdate(ctx, tx.WalletID())
		if err != nil {
			return err
		}
		out, err := s.decide(ctx, r, tx, w, now)
		if err != nil {
			return err
		}
		if tx.Status() == wager.PendingReference {
			// decide não muda uma operação que já espera: ainda sem referência.
			if !now.Before(*tx.ExpiresAt()) {
				if err := tx.Reject(wager.ReferenceNotFound, w.Balance(), nil, now); err != nil {
					return err
				}
			} else {
				if err := tx.ScheduleRetry(now.Add(RetryDelay(tx.Attempts()+2)), now); err != nil {
					return err
				}
				return r.Transactions.Update(ctx, tx)
			}
		}
		return s.persist(ctx, r, tx, w, out, Metadata{}, now)
	})
	return res, classify(err)
}

// MarkFailed registra falha permanente de uma pendência, numa transação
// separada da que falhou. Só vale para PENDING_REFERENCE.
func (s *Service) MarkFailed(ctx context.Context, tx *wager.Transaction) error {
	err := s.tx.WithinTx(ctx, func(ctx context.Context, r store.Repos) error {
		current, err := r.Transactions.GetByID(ctx, tx.ID())
		if err != nil {
			return err
		}
		if current.Status() != wager.PendingReference {
			return nil
		}
		if err := current.Fail(PermanentFailure, s.clock.Now()); err != nil {
			return err
		}
		return r.Transactions.Update(ctx, current)
	})
	return classify(err)
}

// IsTransient informa se o erro é uma falha transitória (repetir depois).
func IsTransient(err error) bool {
	return apperr.IsKind(err, apperr.Unavailable) || errors.Is(err, context.DeadlineExceeded)
}
