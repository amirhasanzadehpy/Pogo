from datetime import date

from myapp.models import Author, Book, Node, Publication


def book_queries(author: Author):
    active = Book.objects.active().published()
    filtered = active.filter(
        author__profile__display_name__icontains="Ada",
        published_at__date__gte=date(2020, 1, 1),
        metadata__featured=True,
    )
    featured = Book.catalog.featured().filter(author__name__startswith="A")
    reverse_filter = Author.objects.filter(book__title__icontains="language")
    projections = Book.objects.values("author__name", "published_at")
    selected = Book.objects.select_related("author__profile")
    prefetched = Book.objects.prefetch_related("store_set")
    authored = author.books.all()
    return (
        filtered,
        featured,
        reverse_filter,
        projections,
        selected,
        prefetched,
        authored,
    )


def implicit_reverse_queries(book: Book):
    stores = Book.objects.filter(store__name__icontains="books")
    stocked_at = book.store_set.all()
    return stores, stocked_at


def recursive_queries(node: Node):
    ancestors = Node.objects.filter(parent__parent__name="root")
    children = Node.objects.filter(node__name="child")
    instance_children = node.node_set.all()
    return ancestors, children, instance_children


def inheritance_queries():
    return Publication.objects.filter(specialedition__edition_number=1)
