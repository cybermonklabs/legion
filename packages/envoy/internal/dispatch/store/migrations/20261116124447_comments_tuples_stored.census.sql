-- comments gains a boolean column with a constant default and no constraint beyond not null. On
-- PostgreSQL 11 and later that is a catalog change: no stored row is read or rewritten, and every
-- existing row reads the default, which no constraint can refuse. The number of existing rows this
-- migration would refuse or rewrite is therefore 0, whatever comments holds.
select 0;
