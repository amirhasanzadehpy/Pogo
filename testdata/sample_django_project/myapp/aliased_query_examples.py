import myapp.models as app_models
from myapp.models import Book as CatalogBook


def qualified_and_aliased_queries():
    qualified = app_models.Book.objects.filter(author__name="Ada")
    aliased = CatalogBook.objects.filter(page_count__gte=100)
    constructed = CatalogBook(title="Draft")
    assigned = constructed
    return qualified, aliased, assigned.author_id
