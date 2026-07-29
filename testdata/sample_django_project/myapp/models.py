from __future__ import annotations

from django.db import models
from django.utils import timezone


class TimeStampedModel(models.Model):
    created_at = models.DateTimeField(
        auto_now_add=True,
        db_index=True,
        help_text="Time when this record was created.",
    )
    updated_at = models.DateTimeField(
        auto_now=True,
        help_text="Time when this record was last updated.",
    )

    class Meta:
        abstract = True


class Author(TimeStampedModel):
    name = models.CharField(
        max_length=200,
        db_index=True,
        help_text="Author's display name.",
    )


class Profile(TimeStampedModel):
    author = models.OneToOneField(
        Author,
        on_delete=models.CASCADE,
        related_name="profile",
        related_query_name="profile",
        help_text="Author represented by this profile.",
    )
    display_name = models.CharField(
        max_length=200,
        help_text="Public name shown for the profile.",
    )


class BookQuerySet(models.QuerySet):
    def active(self):
        """Return books that are currently active."""
        return self.filter(is_active=True)

    def published(self):
        """Return active books whose publication date has passed."""
        return self.active().filter(
            published_at__isnull=False,
            published_at__lte=timezone.now(),
        )


class BookManager(models.Manager):
    def featured(self) -> models.QuerySet["Book"]:
        """Return books marked as featured in their JSON metadata."""
        return self.get_queryset().filter(metadata__featured=True)


class Book(TimeStampedModel):
    author = models.ForeignKey(
        Author,
        on_delete=models.CASCADE,
        related_name="books",
        related_query_name="book",
        help_text="Author who wrote the book.",
    )
    title = models.CharField(
        max_length=255,
        help_text="Published title of the book.",
    )
    summary = models.TextField(
        blank=True,
        help_text="Optional plain-text book summary.",
    )
    published_at = models.DateTimeField(
        null=True,
        blank=True,
        help_text="Publication time, or null for unpublished books.",
    )
    metadata = models.JSONField(
        default=dict,
        blank=True,
        help_text="Structured publishing metadata.",
    )
    page_count = models.IntegerField(
        default=0,
        help_text="Number of pages in the book.",
    )
    is_active = models.BooleanField(
        default=True,
        db_index=True,
        help_text="Whether the book is active in the catalog.",
    )

    objects = BookQuerySet.as_manager()
    catalog = BookManager()

    class Meta:
        indexes = [
            models.Index(
                fields=["author", "published_at"],
                name="book_author_pub_idx",
            ),
        ]


class Store(TimeStampedModel):
    name = models.CharField(
        max_length=200,
        unique=True,
        help_text="Unique store name.",
    )
    books = models.ManyToManyField(
        Book,
        blank=True,
        help_text="Books stocked by this store.",
    )


class Publication(TimeStampedModel):
    title = models.CharField(
        max_length=255,
        help_text="Title shared by publication variants.",
    )


class SpecialEdition(Publication):
    edition_number = models.IntegerField(
        help_text="Number identifying this special edition.",
    )


class Node(TimeStampedModel):
    name = models.CharField(
        max_length=200,
        help_text="Node label.",
    )
    parent = models.ForeignKey(
        "self",
        on_delete=models.SET_NULL,
        null=True,
        blank=True,
        help_text="Optional parent node.",
    )
