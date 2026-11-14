-- 20261113090412 adds a function and a trigger on events, which run only on later inserts: it
-- refuses and rewrites no existing row. The boot's fill, whose progress row 20261113090413
-- creates (store.FillSessionTitles), is what covers the events already stored.
select 0;
