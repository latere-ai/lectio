// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build race

package extract

// slowdown is how many times slower than a plain build a test allows the
// validator to be: the race detector follows every read and write.
const slowdown = 12
