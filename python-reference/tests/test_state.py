import pytest

from app.models import Trip, TripState as S
from app.state import InvalidTransition, assert_can_book, check_transition


def test_happy_path_transitions_allowed():
    path = [S.COLLECTING, S.AWAITING_CHOICE, S.SEARCHING, S.AWAITING_APPROVAL, S.BOOKING, S.CONFIRMED]
    for a, b in zip(path, path[1:]):
        check_transition(a, b)


@pytest.mark.parametrize("a,b", [
    (S.COLLECTING, S.BOOKING),          # can't skip approval
    (S.AWAITING_CHOICE, S.BOOKING),
    (S.CONFIRMED, S.COLLECTING),        # terminal
    (S.BOOKING, S.CANCELLED),           # don't abandon mid-booking
])
def test_illegal_transitions(a, b):
    with pytest.raises(InvalidTransition):
        check_transition(a, b)


def test_cannot_book_without_approval():
    with pytest.raises(InvalidTransition):
        assert_can_book(Trip(id="t", group_id="g", state=S.BOOKING, approved_by=None))
    with pytest.raises(InvalidTransition):
        assert_can_book(Trip(id="t", group_id="g", state=S.AWAITING_APPROVAL, approved_by="Jordan"))
    assert_can_book(Trip(id="t", group_id="g", state=S.BOOKING, approved_by="Jordan"))
