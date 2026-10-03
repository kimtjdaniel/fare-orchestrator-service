from fastapi.testclient import TestClient

from app.main import app, telegram_to_incoming


def test_webhook_end_to_end(transcript):
    with TestClient(app) as client:
        assert client.get("/health").json()["store"] == "memory"
        for t in transcript["messages"] + transcript["followups"]:
            body = {"group_id": transcript["group_id"], "group_name": transcript["group_name"],
                    "tagged": False, "timestamp": 0, **t}
            r = client.post("/webhook", json=body)
            assert r.status_code == 200 and r.json()["accepted"]

        outbox = app.state.brain.messenger.outbox
        assert any("Booked" in m["text"] for m in outbox)
        # trip is confirmed, so no active trip; read it back by id from the store
        trip_id = next(iter(app.state.brain.store.trips))
        view = client.get(f"/trips/{trip_id}").json()
        assert view["trip"]["state"] == "CONFIRMED"
        assert len(view["bookings"]) == 4


def test_telegram_mention_parsing():
    update = {"update_id": 1, "message": {
        "message_id": 7, "date": 1790000000,
        "chat": {"id": -100123, "type": "supergroup", "title": "Fall trip"},
        "from": {"id": 55, "first_name": "Jordan"},
        "text": "@fare_bot figure this out",
        "entities": [{"type": "mention", "offset": 0, "length": 9}]}}
    m = telegram_to_incoming(update)
    assert m.tagged and m.group_id == "-100123" and m.sender_name == "Jordan" and m.message_id == "7"


def test_telegram_button_press():
    update = {"update_id": 2, "callback_query": {
        "id": "cbq1", "data": "✅", "from": {"id": 55, "first_name": "Jordan"},
        "message": {"message_id": 9, "chat": {"id": -100123, "type": "supergroup", "title": "Fall trip"}}}}
    m = telegram_to_incoming(update)
    assert m.text == "✅" and m.tagged


def test_telegram_ignores_private_chats():
    update = {"update_id": 3, "message": {"message_id": 1, "date": 0, "text": "hi",
              "chat": {"id": 5, "type": "private"}, "from": {"id": 5, "first_name": "X"}}}
    assert telegram_to_incoming(update) is None
