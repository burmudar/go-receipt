package db

type Store[T any] interface {
	Create(v *T) error
	Get(id int) (*T, error)
	List() ([]*T, error)
	Delete(id int) (*T, error)
	Update(v *T) error
}
