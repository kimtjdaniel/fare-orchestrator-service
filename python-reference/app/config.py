"""All settings in one place, read from environment / .env. Defaults = fully mocked, no keys needed."""
import os
from dataclasses import dataclass

from dotenv import load_dotenv

load_dotenv()


def _bool(name: str, default: bool) -> bool:
    return os.getenv(name, str(default)).strip().lower() in ("1", "true", "yes", "on")


@dataclass
class Settings:
    mock_llm: bool = _bool("MOCK_LLM", True)
    mock_travel: bool = _bool("MOCK_TRAVEL", True)
    mock_browser: bool = _bool("MOCK_BROWSER", True)
    mock_booking_delay: float = float(os.getenv("MOCK_BOOKING_DELAY", "3"))

    anthropic_api_key: str = os.getenv("ANTHROPIC_API_KEY", "")
    anthropic_model: str = os.getenv("ANTHROPIC_MODEL", "claude-sonnet-5-5")
    llm_cache_dir: str = os.getenv("LLM_CACHE_DIR", ".llm_cache")

    database_url: str = os.getenv("DATABASE_URL", "")

    messaging_backend: str = os.getenv("MESSAGING_BACKEND", "console")
    robot_url: str = os.getenv("ROBOT_URL", "http://localhost:3000")
    telegram_bot_token: str = os.getenv("TELEGRAM_BOT_TOKEN", "")
    telegram_bot_username: str = os.getenv("TELEGRAM_BOT_USERNAME", "")
    telegram_webhook_secret: str = os.getenv("TELEGRAM_WEBHOOK_SECRET", "")
    bot_name: str = os.getenv("BOT_NAME", "Yate")
    timezone: str = os.getenv("TIMEZONE", "America/Vancouver")

    skyvern_api_key: str = os.getenv("SKYVERN_API_KEY", "")
    hotel_checkout_url: str = os.getenv("HOTEL_CHECKOUT_URL", "https://example-fake-hotel.vercel.app")
    skyvern_max_steps: int = int(os.getenv("SKYVERN_MAX_STEPS", "25"))

    dashboard_url: str = os.getenv("DASHBOARD_URL", "http://localhost:3001")


settings = Settings()
