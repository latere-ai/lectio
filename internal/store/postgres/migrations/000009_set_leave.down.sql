-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- Back to the schema of 000008: a parse leaves a set only by being deleted.
-- An extraction that left its set stays an extraction of no set.
DROP FUNCTION lectio_set_leave(text, text, timestamptz);
