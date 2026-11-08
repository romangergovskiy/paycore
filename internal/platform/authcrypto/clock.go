package authcrypto

import (
	"time"

	"github.com/romangergovskiy/paycore/internal/auth/application"
)

var _ application.Clock = SystemClock{}

// SystemClock is application.Clock using the process wall clock in UTC.
type SystemClock struct{}

func NewSystemClock() SystemClock { return SystemClock{} }

func (SystemClock) Now() time.Time { return time.Now().UTC() }
