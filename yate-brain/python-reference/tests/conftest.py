import json
import os
import sys
from pathlib import Path

# Force offline mode before the app reads its settings. Tests never spend credits.
os.environ.update({
    "MOCK_LLM": "true", "MOCK_TRAVEL": "true", "MOCK_BROWSER": "true",
    "MOCK_BOOKING_DELAY": "0", "MESSAGING_BACKEND": "console", "DATABASE_URL": "",
    "TELEGRAM_BOT_USERNAME": "yate_bot", "TELEGRAM_BOT_TOKEN": "", "TELEGRAM_WEBHOOK_SECRET": "",
})
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pytest  # noqa: E402

FIXTURES = Path(__file__).parent / "fixtures"


@pytest.fixture
def transcript() -> dict:
    return json.loads((FIXTURES / "demo_transcript.json").read_text())
