"""The ways a request can fail that a caller can act on.

Anything else that goes wrong is a fault in the service, and is reported as
one.
"""

from enum import StrEnum


class Reason(StrEnum):
    """Why a text cannot be processed. The value is what the caller is sent."""

    TOO_LONG = "too_long"
    """The text is longer than the embedding model is given."""

    TITLE_TOO_LONG = "title_too_long"
    """The title is so long that it leaves no room for the document."""

    UNFINISHED = "unfinished"
    """The model ran out of room before it had finished writing."""

    NOTHING_WRITTEN = "nothing_written"
    """The model wrote nothing."""

    OFF_FORM = "off_form"
    """The model's reply did not have the form it was asked for.

    A model that has been talked out of its instructions by the document it
    was reading usually gives itself away like this.
    """

    TOOK_TOO_LONG = "took_too_long"
    """The document is more than the model can get through in the time allowed."""


class CannotProcess(Exception):
    """This text cannot be processed, and asking again will not help.

    The fault is neither the caller's nor the service's: it lies in the text,
    or in what the model made of it. The services answer it with a 422 that
    carries the reason, and the caller goes on without the embedding or the
    summary rather than trying for ever.
    """

    def __init__(self, reason: Reason, message: str) -> None:
        super().__init__(message)
        self.reason = reason
        self.message = message


class BadRequest(Exception):
    """The caller sent something that makes no sense, such as no text at all.

    Unlike CannotProcess this is a mistake in the calling program, so the
    services answer it with a 400 and the caller treats it as a failure.
    """
