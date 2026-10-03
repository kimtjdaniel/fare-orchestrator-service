from pathlib import Path

from dotenv import dotenv_values


def test_env_example_has_no_comment_leaking_into_values():
    values = dotenv_values(Path(__file__).resolve().parent.parent / ".env.example")
    leaked = {k: v for k, v in values.items() if v and "#" in v}
    assert not leaked, f"inline comments parsed as values: {leaked}"
