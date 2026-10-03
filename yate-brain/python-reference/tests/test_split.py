from app.tools.split import compute_split


def test_split_each_pays_own_flight_plus_hotel_share():
    s = compute_split({"Maya": 300, "Jordan": 500, "Sam": 250}, hotel_total=600, payer="Jordan")
    assert s["per_person"] == {"Maya": 500, "Jordan": 700, "Sam": 450}
    assert s["group_total"] == 1650
    assert {(o["from"], o["amount"]) for o in s["owes"]} == {("Maya", 500), ("Sam", 450)}
    assert all(o["to"] == "Jordan" for o in s["owes"])


def test_split_pennies_add_up():
    s = compute_split({"A": 100, "B": 100, "C": 100}, hotel_total=100, payer="A")
    assert round(sum(s["per_person"].values()), 2) == s["group_total"] == 400


def test_split_empty():
    assert compute_split({}, 0, "x")["owes"] == []
